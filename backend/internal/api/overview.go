package api

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// overview reads what the public landing page shows: three program figures and
// the species confirmed in the last `days` days. Nothing in it names a
// volunteer or a card.
//
// Every count happens here in Go, not in a query: Cosmos can't aggregate across
// partitions from the Go SDK (see SCHEMA.md). If this gets expensive as
// detections pile up, it becomes a precomputed summary document.
func overview(ctx context.Context, store db.Store, now time.Time, days int) (ProgramStats, []Species, error) {
	yearStart := time.Date(now.In(pacific).Year(), time.January, 1, 0, 0, 0, 0, pacific)
	windowStart := now.AddDate(0, 0, -days)

	recorders, err := store.ListRecorders(ctx)
	if err != nil {
		return ProgramStats{}, nil, err
	}
	uploads, err := store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return ProgramStats{}, nil, err
	}
	// One query covers both the year's count and the window, which reaches back
	// into last year early in January.
	since := yearStart
	if windowStart.Before(since) {
		since = windowStart
	}
	confirmed, err := store.ListDetections(ctx, db.DetectionFilter{ReviewStatus: db.ReviewConfirmed, Since: since})
	if err != nil {
		return ProgramStats{}, nil, err
	}

	var program ProgramStats
	for _, r := range recorders {
		if r.RetiredAt == nil {
			program.Recorders++
		}
	}
	// A recorder-night is counted once however many cards list it.
	year := yearStart.Format("2006-")
	nights := map[[2]string]bool{}
	stationName := map[string]string{}
	for _, u := range uploads {
		stationName[u.ID] = u.Recorder.Name
		for _, n := range u.Nights {
			if strings.HasPrefix(n.Date, year) {
				nights[[2]string{u.RecorderID, n.Date}] = true
			}
		}
	}
	program.NightsRecorded = len(nights)

	type tally struct {
		Species
		nights   map[string]bool
		stations map[string]int
	}
	bySpecies := map[string]*tally{}
	for _, d := range confirmed {
		if !d.DetectedAt.Before(yearStart) {
			program.ConfirmedDetections++
		}
		if d.DetectedAt.Before(windowStart) {
			continue
		}
		scientific, common := d.Species()
		t := bySpecies[scientific]
		if t == nil {
			t = &tally{
				Species:  Species{ScientificName: scientific, CommonName: common},
				nights:   map[string]bool{},
				stations: map[string]int{},
			}
			bySpecies[scientific] = t
		}
		t.Detections++
		t.nights[d.Night] = true
		// Where the card says it was recorded, not where the unit is now.
		name := stationName[d.UploadID]
		if name == "" {
			name = d.RecorderID
		}
		t.stations[name]++
		if d.DetectedAt.After(t.LastDetectedAt) {
			t.LastDetectedAt = d.DetectedAt
		}
	}

	species := make([]Species, 0, len(bySpecies))
	for _, t := range bySpecies {
		t.Nights = len(t.nights)
		t.Stations = make([]string, 0, len(t.stations))
		for name := range t.stations {
			t.Stations = append(t.Stations, name)
		}
		// The station that hears it most comes first.
		slices.SortFunc(t.Stations, func(a, b string) int {
			return cmp.Or(cmp.Compare(t.stations[b], t.stations[a]), cmp.Compare(a, b))
		})
		species = append(species, t.Species)
	}
	slices.SortFunc(species, func(a, b Species) int {
		return cmp.Or(cmp.Compare(b.Detections, a.Detections), cmp.Compare(a.CommonName, b.CommonName))
	})
	return program, species, nil
}
