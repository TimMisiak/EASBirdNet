// Command bouts measures what bouting would make of real cards, for choosing
// the review rules' bout gap (REVIEW.md, *Measure before tuning*).
//
// It reads detections as the MCP detections_export tool writes them (CSV,
// one row per detection) and a review rules table as the Review rules page
// exports it, routes each detection by the rules and bouts them exactly as
// the preparer would (internal/review), at several gaps, and reports per
// card how many bouts reviewers would have to hear at each:
//
//	cd backend
//	go run ./cmd/bouts -rules ../frontend/data/suggested-review-rules.json export.csv
//
// The gap worth picking is the one past which bouts stop getting fewer much
// faster: beyond it a bout mostly swallows calls that were unrelated.
//
// Every row is routed as if no one had reviewed it, since the export says
// what a detection's status is but not whether a person gave it.
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/review"
)

func main() {
	rulesPath := flag.String("rules", "", "a review rules table, as JSON (required)")
	gapsFlag := flag.String("gaps", "5,15,30,60", "bout gaps to compare, in minutes")
	top := flag.Int("species", 10, "also list this many species with the most review bouts at each gap")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s -rules RULES.json [flags] EXPORT.csv...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *rulesPath == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	gaps, err := parseGaps(*gapsFlag)
	if err == nil {
		var table *review.Table
		if table, err = readRules(*rulesPath); err == nil {
			var cards map[string][]db.Detection
			if cards, err = readExports(flag.Args()); err == nil {
				report(os.Stdout, table, cards, gaps, *top)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bouts:", err)
		os.Exit(1)
	}
}

func parseGaps(s string) ([]time.Duration, error) {
	var gaps []time.Duration
	for _, f := range strings.Split(s, ",") {
		m, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || m < 1 {
			return nil, fmt.Errorf("-gaps: %q is not a number of minutes", f)
		}
		gaps = append(gaps, time.Duration(m)*time.Minute)
	}
	return gaps, nil
}

func readRules(path string) (*review.Table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules db.ReviewRules
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := review.Check(&rules); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return review.NewTable(&rules), nil
}

// readExports reads detections_export CSVs into each card's detections.
func readExports(paths []string) (map[string][]db.Detection, error) {
	cards := map[string][]db.Detection{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		err = readExport(f, cards)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return cards, nil
}

func readExport(r io.Reader, cards map[string][]db.Detection) error {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, name := range header {
		col[name] = i
	}
	for _, name := range []string{"id", "card", "recorderId", "heardAt", "startSec", "endSec", "birdnetScientificName", "birdnetCommonName", "confidence"} {
		if _, ok := col[name]; !ok {
			return fmt.Errorf("no %s column: is this a detections_export file?", name)
		}
	}
	for line := 2; ; line++ {
		row, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		d := db.Detection{
			ID: row[col["id"]], UploadID: row[col["card"]], RecorderID: row[col["recorderId"]],
			ScientificName: row[col["birdnetScientificName"]], CommonName: row[col["birdnetCommonName"]],
			ReviewStatus: db.ReviewUnreviewed,
		}
		var errs [4]error
		d.DetectedAt, errs[0] = time.Parse(time.RFC3339, row[col["heardAt"]])
		d.StartSec, errs[1] = strconv.ParseFloat(row[col["startSec"]], 64)
		d.EndSec, errs[2] = strconv.ParseFloat(row[col["endSec"]], 64)
		d.Confidence, errs[3] = strconv.ParseFloat(row[col["confidence"]], 64)
		if err := errors.Join(errs[:]...); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		cards[d.UploadID] = append(cards[d.UploadID], d)
	}
}

// measured is one card at one gap.
type measured struct {
	gap                   time.Duration
	reviewBouts, singles  int
	median, largest       int
	spotChecks, clips     int
	largestCheck          int
	perSpecies            map[string]int
	reviewed, accepted, n int
}

func measure(card db.Upload, table *review.Table, detections []db.Detection, gap time.Duration) measured {
	var toReview, accepted []review.Member
	for _, d := range detections {
		table.Apply(&d)
		switch {
		case d.Route == db.RouteReview:
			toReview = append(toReview, review.MemberOf(d))
		case d.ReviewStatus == db.ReviewAccepted:
			accepted = append(accepted, review.MemberOf(d))
		}
	}
	bouts := review.Bouts(card, db.BoutReview, toReview, gap)
	checks := review.SpotChecks(review.Bouts(card, db.BoutSpotCheck, accepted, gap), table.Rules.SpotChecksPerSpecies, nil)
	m := measured{gap: gap, reviewBouts: len(bouts), spotChecks: len(checks), perSpecies: map[string]int{},
		reviewed: len(toReview), accepted: len(accepted), n: len(detections)}
	var sizes []int
	for _, b := range append(bouts, checks...) {
		m.clips += len(b.Best)
	}
	for _, b := range checks {
		m.largestCheck = max(m.largestCheck, len(b.DetectionIDs))
	}
	for _, b := range bouts {
		sizes = append(sizes, len(b.DetectionIDs))
		if len(b.DetectionIDs) == 1 {
			m.singles++
		}
		m.perSpecies[b.CommonName]++
	}
	if len(sizes) > 0 {
		slices.Sort(sizes)
		m.median, m.largest = sizes[len(sizes)/2], sizes[len(sizes)-1]
	}
	return m
}

func report(w io.Writer, table *review.Table, cards map[string][]db.Detection, gaps []time.Duration, top int) {
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range cards {
			if !yield(id) {
				return
			}
		}
	})
	for _, id := range ids {
		detections := cards[id]
		card := db.Upload{ID: id, RecorderID: detections[0].RecorderID}
		var ms []measured
		for _, gap := range gaps {
			ms = append(ms, measure(card, table, detections, gap))
		}
		fmt.Fprintf(w, "%s (%s): %d detections; by the rules, %d to review and %d accepted; %d spot-checks a species\n\n",
			id, card.RecorderID, ms[0].n, ms[0].reviewed, ms[0].accepted, table.Rules.SpotChecksPerSpecies)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "gap\treview bouts\tvs. last\tof 1 detection\tmedian size\tlargest\tspot-checks\tlargest\tclips to hear\t")
		for i, m := range ms {
			fewer := ""
			if i > 0 && ms[i-1].reviewBouts > 0 {
				fewer = fmt.Sprintf("%+.0f%%", 100*float64(m.reviewBouts-ms[i-1].reviewBouts)/float64(ms[i-1].reviewBouts))
			}
			fmt.Fprintf(tw, "%v\t%d\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t\n", m.gap, m.reviewBouts, fewer, m.singles, m.median, m.largest, m.spotChecks, m.largestCheck, m.clips)
		}
		tw.Flush()
		if top > 0 {
			fmt.Fprintf(w, "\nReview bouts of the %d species with the most:\n\n", top)
			species := make([]string, 0, len(ms[0].perSpecies))
			for s := range ms[0].perSpecies {
				species = append(species, s)
			}
			slices.SortFunc(species, func(a, b string) int {
				return cmpThen(ms[0].perSpecies[b]-ms[0].perSpecies[a], strings.Compare(a, b))
			})
			tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
			fmt.Fprint(tw, "species\t")
			for _, m := range ms {
				fmt.Fprintf(tw, "%v\t", m.gap)
			}
			fmt.Fprintln(tw)
			for _, s := range species[:min(top, len(species))] {
				fmt.Fprintf(tw, "%s\t", s)
				for _, m := range ms {
					fmt.Fprintf(tw, "%d\t", m.perSpecies[s])
				}
				fmt.Fprintln(tw)
			}
			tw.Flush()
		}
		fmt.Fprintln(w)
	}
}

func cmpThen(cs ...int) int {
	for _, c := range cs {
		if c != 0 {
			return c
		}
	}
	return 0
}
