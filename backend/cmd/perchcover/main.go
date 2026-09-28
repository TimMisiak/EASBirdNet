// Command perchcover measures what running Perch over less audio would save
// and what it would cost (ANALYSIS.md, *Open: how much audio Perch hears*).
//
// It runs BirdNET and Perch over the same files -- real card files, with the
// recorder's location and date as the server would -- and reports:
//
//   - how much of the audio Perch would still have to hear if it only ran
//     around what BirdNET heard: all of BirdNET's detections, only the birds
//     (BirdNET also reports engines, dogs and people), or only the birds it was
//     unsure of; each with a margin either side, rounded out to Perch's
//     5-second windows. That is the saving.
//   - what that Perch would miss: its detections, and the species, that fall
//     outside that audio when it hears everything. That is the cost.
//
// Perch is slow (~4x real time on two cores), so the runs are saved to -out
// and the report can be made again from them with -in, without running
// anything:
//
//	cd backend
//	BIRDSENSE_BIRDNET_PYTHON=../.venv/bin/python go run ./cmd/perchcover \
//	  -lat 47.66 -lon -122.11 -date 2026-09-12 -out cover.json CARD/DATA/20260912/*.WAV
//	go run ./cmd/perchcover -in cover.json
//
// Keeping only the Perch windows that overlap the kept audio approximates a
// Perch run over just that audio: a real one would place its windows from
// the start of each stretch rather than on the file's 5-second grid. For a
// share of the audio and of the detections, that is close enough.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
)

// saved is what -out writes and -in reads: both runs, and each file's length.
type saved struct {
	Files   []file         `json:"files"`
	BirdNET birdnet.Result `json:"birdnet"`
	Perch   birdnet.Result `json:"perch"`
}

type file struct {
	Path        string  `json:"path"`
	DurationSec float64 `json:"durationSec"`
}

func main() {
	a := birdnet.Analyzer{Stderr: os.Stderr}
	var lat, lon, date, in, out string
	var threads int
	flag.StringVar(&a.Python, "python", envOr("BIRDSENSE_BIRDNET_PYTHON", "../.venv/bin/python"), "Python with analyzer/requirements.txt and requirements-perch.txt (env BIRDSENSE_BIRDNET_PYTHON)")
	flag.StringVar(&a.Script, "script", envOr("BIRDSENSE_BIRDNET_SCRIPT", "../analyzer/analyze.py"), "path to analyze.py (env BIRDSENSE_BIRDNET_SCRIPT)")
	flag.StringVar(&lat, "lat", "", "the recorder's latitude, as the server would use it")
	flag.StringVar(&lon, "lon", "", "its longitude")
	flag.StringVar(&date, "date", "", "YYYY-MM-DD the files were recorded, for the week")
	flag.IntVar(&threads, "threads", 0, "TensorFlow threads for Perch; 0 is every core")
	flag.StringVar(&in, "in", "", "report from runs saved earlier instead of running the models")
	flag.StringVar(&out, "out", "perchcover.json", "where to save the runs")
	verbose := flag.Bool("v", false, "also list each file, and the species each option would miss")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] FILE...\n       %s -in SAVED.json\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	var s saved
	if in != "" {
		b, err := os.ReadFile(in)
		if err == nil {
			err = json.Unmarshal(b, &s)
		}
		if err != nil {
			fail(err.Error())
		}
	} else {
		if flag.NArg() == 0 {
			flag.Usage()
			os.Exit(2)
		}
		opts := birdnet.Options{}
		if lat != "" || lon != "" {
			var loc birdnet.Location
			var err1, err2 error
			loc.Latitude, err1 = strconv.ParseFloat(lat, 64)
			loc.Longitude, err2 = strconv.ParseFloat(lon, 64)
			if err1 != nil || err2 != nil {
				fail("-lat and -lon must both be numbers")
			}
			if date != "" {
				d, err := time.Parse(time.DateOnly, date)
				if err != nil {
					fail("-date must be YYYY-MM-DD")
				}
				loc.Week = birdnet.Week(d)
			}
			opts.Location = &loc
		} else {
			fmt.Fprintln(os.Stderr, "perchcover: no -lat/-lon, so both models consider every species; the server narrows them to the recorder's location")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		var err error
		if s, err = runBoth(ctx, a, opts, threads, flag.Args()); err != nil {
			fail(err.Error())
		}
		b, _ := json.MarshalIndent(s, "", " ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			fail(err.Error())
		}
		fmt.Fprintf(os.Stderr, "perchcover: runs saved to %s; report again with -in %s\n", out, out)
	}
	report(os.Stdout, s, *verbose)
}

// runBoth runs each model once over every file, so each loads once, and reads
// each file's length.
func runBoth(ctx context.Context, a birdnet.Analyzer, opts birdnet.Options, threads int, paths []string) (saved, error) {
	var s saved
	for _, p := range paths {
		rec, err := a.Cut(ctx, p, nil)
		if err != nil {
			return s, fmt.Errorf("reading %s: %w", p, err)
		}
		s.Files = append(s.Files, file{Path: p, DurationSec: rec.DurationSec})
	}
	var total float64
	for _, f := range s.Files {
		total += f.DurationSec
	}
	fmt.Fprintf(os.Stderr, "perchcover: %d files, %s of audio; BirdNET first\n", len(s.Files), hours(total))
	var err error
	began := time.Now()
	if s.BirdNET, err = a.Analyze(ctx, paths, opts); err != nil {
		return s, err
	}
	fmt.Fprintf(os.Stderr, "perchcover: BirdNET took %s; now Perch, which takes ~17x as much CPU\n", time.Since(began).Round(time.Second))
	began = time.Now()
	p := opts
	p.Model, p.Threads = birdnet.ModelPerch, threads
	if s.Perch, err = a.Analyze(ctx, paths, p); err != nil {
		return s, err
	}
	fmt.Fprintf(os.Stderr, "perchcover: Perch took %s\n", time.Since(began).Round(time.Second))
	return s, nil
}

// perchWindow is the length of Perch's windows, which the kept audio is
// rounded out to: Perch can't hear less than a window.
const perchWindow = 5.0

// A selection is which of BirdNET's detections Perch would run around.
type selection struct {
	name string
	keep func(d birdnet.Detection) bool
}

var selections = []selection{
	{"every BirdNET detection", func(birdnet.Detection) bool { return true }},
	{"birds only", isBird},
	{"birds under 0.75", func(d birdnet.Detection) bool { return isBird(d) && d.Confidence < 0.75 }},
	{"birds under 0.5", func(d birdnet.Detection) bool { return isBird(d) && d.Confidence < 0.5 }},
}

var margins = []float64{0, 2, 5}

// isBird tells BirdNET's birds from its other classes -- "Engine", "Dog",
// "Human vocal" -- which are labelled with the same name twice, where a bird
// has a scientific and a common name.
func isBird(d birdnet.Detection) bool {
	return d.ScientificName != d.CommonName
}

// span is a stretch of a file, in seconds.
type span struct{ start, end float64 }

// kept is the audio Perch would hear in one file: the stretches around the
// selected detections, widened by margin, rounded out to Perch's window grid,
// clipped to the file, and merged.
func kept(ds []birdnet.Detection, keep func(birdnet.Detection) bool, margin, duration float64) []span {
	var spans []span
	for _, d := range ds {
		if !keep(d) {
			continue
		}
		s := math.Floor((d.StartSec-margin)/perchWindow) * perchWindow
		e := math.Ceil((d.EndSec+margin)/perchWindow) * perchWindow
		s = max(s, 0)
		if duration > 0 {
			e = min(e, duration)
		}
		if e > s {
			spans = append(spans, span{s, e})
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return cmpFloat(a.start, b.start) })
	var merged []span
	for _, s := range spans {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, s.end)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

func length(spans []span) float64 {
	var t float64
	for _, s := range spans {
		t += s.end - s.start
	}
	return t
}

// inside reports whether a detection overlaps any of the spans.
func inside(d birdnet.Detection, spans []span) bool {
	for _, s := range spans {
		if d.StartSec < s.end && d.EndSec > s.start {
			return true
		}
	}
	return false
}

// outcome is one selection at one margin, over every file.
type outcome struct {
	keptSec float64
	// perchIn and perchOut count Perch's windows inside and outside the kept
	// audio; lost are the species Perch heard, but never inside it.
	perchIn, perchOut int
	lost              []string
}

func measure(s saved, sel selection, margin float64) outcome {
	birdnetBy := byPath(s.BirdNET)
	perchBy := byPath(s.Perch)
	var o outcome
	heardIn, heard := map[string]bool{}, map[string]bool{}
	for _, f := range s.Files {
		spans := kept(birdnetBy[f.Path], sel.keep, margin, f.DurationSec)
		o.keptSec += length(spans)
		for _, d := range perchBy[f.Path] {
			heard[d.ScientificName] = true
			if inside(d, spans) {
				o.perchIn++
				heardIn[d.ScientificName] = true
			} else {
				o.perchOut++
			}
		}
	}
	for sp := range heard {
		if !heardIn[sp] {
			o.lost = append(o.lost, sp)
		}
	}
	slices.Sort(o.lost)
	return o
}

func byPath(r birdnet.Result) map[string][]birdnet.Detection {
	out := map[string][]birdnet.Detection{}
	for _, f := range r.Files {
		out[f.Path] = f.Detections
	}
	return out
}

func report(w io.Writer, s saved, verbose bool) {
	var total float64
	for _, f := range s.Files {
		total += f.DurationSec
	}
	var bn, birds, pw int
	for _, f := range s.BirdNET.Files {
		for _, d := range f.Detections {
			bn++
			if isBird(d) {
				birds++
			}
		}
	}
	for _, f := range s.Perch.Files {
		pw += len(f.Detections)
	}
	fmt.Fprintf(w, "%d files, %s of audio. BirdNET: %d detection windows (%d of birds). Perch over all of it: %d windows.\n",
		len(s.Files), hours(total), bn, birds, pw)
	if total == 0 {
		return
	}
	names := marginNames(margins)

	results := make([][]outcome, len(selections))
	for i, sel := range selections {
		for _, m := range margins {
			results[i] = append(results[i], measure(s, sel, m))
		}
	}

	fmt.Fprintln(w, "\nThe saving: share of the audio Perch would still hear, running only around what BirdNET heard")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AROUND\t"+strings.Join(names, "\t"))
	for i, sel := range selections {
		row := []string{sel.name}
		for _, o := range results[i] {
			row = append(row, fmt.Sprintf("%.0f%%", 100*o.keptSec/total))
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nThe cost: share of Perch's detection windows that would be missed, and species it would never hear")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AROUND\t"+strings.Join(names, "\t"))
	for i, sel := range selections {
		row := []string{sel.name}
		for _, o := range results[i] {
			missed := "-"
			if n := o.perchIn + o.perchOut; n > 0 {
				missed = fmt.Sprintf("%.0f%%, %d species", 100*float64(o.perchOut)/float64(n), len(o.lost))
			}
			row = append(row, missed)
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()

	agreement(w, s)

	if verbose {
		fmt.Fprintln(w, "\nSpecies each option would never hear from Perch")
		for i, sel := range selections {
			for j, m := range margins {
				if lost := results[i][j].lost; len(lost) > 0 {
					fmt.Fprintf(w, "  %s, %g s: %s\n", sel.name, m, strings.Join(lost, ", "))
				}
			}
		}
		fmt.Fprintln(w, "\nPer file: share of it Perch would hear around BirdNET's birds, 2 s margin")
		birdnetBy := byPath(s.BirdNET)
		for _, f := range s.Files {
			if f.DurationSec > 0 {
				k := length(kept(birdnetBy[f.Path], isBird, 2, f.DurationSec))
				fmt.Fprintf(w, "  %3.0f%%  %s\n", 100*k/f.DurationSec, filepath.Base(f.Path))
			}
		}
	}
}

func marginNames(ms []float64) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = fmt.Sprintf("±%g s", m)
	}
	return out
}

// agreement is which species each model heard at all, over every file: the
// context for what a second opinion adds.
func agreement(w io.Writer, s saved) {
	species := func(r birdnet.Result, birdsOnly bool) map[string]bool {
		out := map[string]bool{}
		for _, f := range r.Files {
			for _, d := range f.Detections {
				if !birdsOnly || isBird(d) {
					out[d.ScientificName] = true
				}
			}
		}
		return out
	}
	b, p := species(s.BirdNET, true), species(s.Perch, false)
	var both, bOnly, pOnly []string
	for sp := range b {
		if p[sp] {
			both = append(both, sp)
		} else {
			bOnly = append(bOnly, sp)
		}
	}
	for sp := range p {
		if !b[sp] {
			pOnly = append(pOnly, sp)
		}
	}
	slices.Sort(bOnly)
	slices.Sort(pOnly)
	fmt.Fprintf(w, "\nSpecies over the whole audio: both models %d, BirdNET only %d, Perch only %d\n", len(both), len(bOnly), len(pOnly))
	if len(pOnly) > 0 {
		fmt.Fprintf(w, "  Perch only: %s\n", strings.Join(pOnly, ", "))
	}
	if len(bOnly) > 0 {
		fmt.Fprintf(w, "  BirdNET only: %s\n", strings.Join(bOnly, ", "))
	}
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func hours(sec float64) string {
	if sec < 3600 {
		return fmt.Sprintf("%.0f min", sec/60)
	}
	return fmt.Sprintf("%.1f h", sec/3600)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "perchcover:", msg)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
