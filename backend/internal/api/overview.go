package api

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/review"
)

// overview reads what the public landing page shows: three program figures and
// the species heard in the last `days` days, counting what the review rules
// accepted and reviewers confirmed. Nothing in it names a volunteer or a card.
//
// It reads each card's summary (review.Build, review.Adjust) rather than the
// detections, which grow by thousands a day per recorder: a year of them is
// more than one read may hold (db.MaxDetectionScan). Counts are by Pacific
// date, so the window is the last `days` dates, today's included. A card with
// no summary yet has one built here, once.
func overview(ctx context.Context, store db.Store, now time.Time, days int) (ProgramStats, []Species, error) {
	year := now.In(pacific).Format("2006-")
	firstDate := now.In(pacific).AddDate(0, 0, 1-days).Format(time.DateOnly)

	recorders, err := store.ListRecorders(ctx)
	if err != nil {
		return ProgramStats{}, nil, err
	}
	uploads, err := store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return ProgramStats{}, nil, err
	}
	stored, err := store.ListCardSummaries(ctx)
	if err != nil {
		return ProgramStats{}, nil, err
	}
	summaries := make(map[string]db.CardSummary, len(stored))
	for _, s := range stored {
		summaries[s.ID] = s
	}

	var program ProgramStats
	for _, r := range recorders {
		if r.RetiredAt == nil {
			program.Recorders++
		}
	}

	type tally struct {
		Species
		days     map[string]bool
		stations map[string]int
	}
	bySpecies := map[string]*tally{}
	for _, u := range uploads {
		// A card being deleted counts for nothing, nor does what was heard on it.
		if u.Status == db.StatusDeleting {
			continue
		}
		// A card's recordings count for the year its first one started in, or
		// that it was pulled in when no file name said.
		started := u.PulledOn
		if u.FirstRecordingAt != nil {
			started = dateOf(*u.FirstRecordingAt)
		}
		if strings.HasPrefix(started, year) {
			program.Recordings += u.FileCount
		}

		summary, ok := summaries[u.ID]
		if !ok {
			if summary, err = review.Build(ctx, store, u.ID); err != nil {
				return ProgramStats{}, nil, err
			}
		}
		// Where the card says it was recorded, not where the unit is now.
		station := cmp.Or(u.Recorder.Name, u.RecorderID)
		for _, s := range summary.Species {
			var t *tally
			for date, n := range s.Days {
				if strings.HasPrefix(date, year) {
					program.Detections += n
				}
				if date < firstDate {
					continue
				}
				if t == nil {
					if t = bySpecies[s.ScientificName]; t == nil {
						t = &tally{
							Species:  Species{ScientificName: s.ScientificName, CommonName: s.CommonName},
							days:     map[string]bool{},
							stations: map[string]int{},
						}
						bySpecies[s.ScientificName] = t
					}
				}
				t.Detections += n
				t.days[date] = true
				t.stations[station] += n
			}
			if t != nil && s.LastDetectedAt.After(t.LastDetectedAt) {
				t.LastDetectedAt = s.LastDetectedAt
			}
		}
	}

	species := make([]Species, 0, len(bySpecies))
	for _, t := range bySpecies {
		t.Days = len(t.days)
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
// every card and every card's summary -- the most expensive read in the app,
// and the one route anyone on the internet can ask for. So each
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
