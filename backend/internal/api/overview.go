package api

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
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

// overviewCache is how often the landing page's scan actually runs.
//
// /public/overview needs no session, and answering it reads every recorder,
// every card and every confirmed detection of the year -- the most expensive
// read in the app, on the one replica that is also running BirdNET. So each
// window's answer is held for the minute it is stamped with, and a flood of
// anonymous requests costs one scan a minute per window size instead of one
// each.
//
// Nothing about the response changes: updatedAt is that same minute, so a
// cached answer is the one a fresh scan would have given. The cost is that
// confirming a detection can take up to a minute to reach the landing page.
//
// An answer is shared by every request that gets it, so nothing may modify
// one after it is stored.
type overviewCache struct {
	// gate admits one scan at a time, so a burst arriving on a cold cache
	// starts one scan rather than one each. It is a channel and not a mutex so
	// a waiter can give up when its own request is cancelled.
	gate chan struct{}

	mu sync.Mutex
	// minute is the minute every answer in answers is the answer for; the map
	// is emptied when it rolls over, so it holds one minute's windows at most.
	minute  time.Time
	answers map[int]overviewAnswer
}

// overviewAnswer is one window's share of the response.
type overviewAnswer struct {
	program ProgramStats
	species []Species
}

func newOverviewCache() *overviewCache {
	return &overviewCache{gate: make(chan struct{}, 1), answers: map[int]overviewAnswer{}}
}

// get is overview, answered from the last scan of this minute where there is
// one. A scan that fails is not an answer: the next request tries again.
func (c *overviewCache) get(ctx context.Context, store db.Store, now time.Time, days int) (ProgramStats, []Species, error) {
	minute := now.Truncate(time.Minute)
	if a, ok := c.lookup(minute, days); ok {
		return a.program, a.species, nil
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ProgramStats{}, nil, ctx.Err()
	}
	defer func() { <-c.gate }()
	// Someone may have scanned this window while we waited for the gate.
	if a, ok := c.lookup(minute, days); ok {
		return a.program, a.species, nil
	}
	program, species, err := overview(ctx, store, now, days)
	if err != nil {
		return ProgramStats{}, nil, err
	}
	c.put(minute, days, overviewAnswer{program: program, species: species})
	return program, species, nil
}

func (c *overviewCache) lookup(minute time.Time, days int) (overviewAnswer, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.minute.Equal(minute) {
		return overviewAnswer{}, false
	}
	a, ok := c.answers[days]
	return a, ok
}

func (c *overviewCache) put(minute time.Time, days int, a overviewAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.minute.Equal(minute) {
		c.minute = minute
		clear(c.answers)
	}
	c.answers[days] = a
}
