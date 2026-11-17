// Package review is what happens to detections after BirdNET: which of them
// count, and the per-card summaries that the public landing page reads instead
// of the detections themselves. REVIEW.md has the design this is part of.
package review

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
)

// pacific is where the recorders are: a summary counts by the date there.
var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// DateOf is the Pacific date an instant falls on, as a summary keys it.
func DateOf(t time.Time) string {
	return t.In(pacific).Format(time.DateOnly)
}

// Build writes a card's summary whole, from its counted detections
// (db.Detection.Counted), and returns it.
//
// A summary is otherwise kept by Adjust, a write at a time. A change that
// lands while Build is reading can be missed, since Build replaces what is
// stored with what it read; the next Build puts that right.
func Build(ctx context.Context, store db.Store, uploadID string) (db.CardSummary, error) {
	s := db.CardSummary{ID: uploadID, BuiltAt: time.Now().UTC()}
	// One status at a time: a filter holds one, and reading every detection
	// on the card to keep a few would read thousands for nothing.
	for _, status := range []string{db.ReviewAccepted, db.ReviewConfirmed} {
		f := db.DetectionFilter{UploadID: uploadID, Model: db.ModelBirdNET, ReviewStatus: status}
		err := store.EachDetection(ctx, f, func(d db.Detection) error {
			add(&s, d, 1)
			return nil
		})
		if err != nil {
			return db.CardSummary{}, err
		}
	}
	if err := store.PutCardSummary(ctx, s); err != nil {
		return db.CardSummary{}, err
	}
	return s, nil
}

// Adjust changes a card's summary by what a write did to its detections:
// before is how they were (nothing, for ones that are new), after is how they
// are now. A card with no summary is left without one, because the first read
// of it builds it whole, and that includes this write.
func Adjust(ctx context.Context, store db.Store, uploadID string, before, after []db.Detection) error {
	var changed []db.Detection
	var signs []int
	for _, d := range before {
		if d.Counted() {
			changed, signs = append(changed, d), append(signs, -1)
		}
	}
	for _, d := range after {
		if d.Counted() {
			changed, signs = append(changed, d), append(signs, 1)
		}
	}
	if cancels(changed, signs) {
		return nil
	}
	_, err := store.UpdateCardSummary(ctx, uploadID, func(s *db.CardSummary) error {
		for i, d := range changed {
			add(s, d, signs[i])
		}
		return nil
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	return err
}

// cancels says whether every count taken away is given back: the same
// species on the same date, as when a confirmed detection is confirmed again.
func cancels(changed []db.Detection, signs []int) bool {
	net := map[string]int{}
	for i, d := range changed {
		scientific, _ := d.Species()
		net[scientific+"\x00"+DateOf(d.DetectedAt)] += signs[i]
	}
	for _, n := range net {
		if n != 0 {
			return false
		}
	}
	return true
}

// add counts n more (or, negative, fewer) of a detection's species on the
// date it was heard. A date that comes to nothing is dropped, as is a species
// with no dates left. Species stay sorted by scientific name, so a summary
// built twice from the same detections is the same document.
func add(s *db.CardSummary, d db.Detection, n int) {
	scientific, common := d.Species()
	i, found := slices.BinarySearchFunc(s.Species, scientific, func(t db.SpeciesTallies, name string) int {
		return strings.Compare(t.ScientificName, name)
	})
	if !found {
		if n <= 0 {
			return
		}
		s.Species = slices.Insert(s.Species, i, db.SpeciesTallies{ScientificName: scientific, CommonName: common, Days: map[string]int{}})
	}
	t := &s.Species[i]
	date := DateOf(d.DetectedAt)
	before := t.Days[date]
	after := max(before+n, 0)
	s.Detections += after - before
	if after == 0 {
		delete(t.Days, date)
	} else {
		t.Days[date] = after
	}
	if n > 0 && d.DetectedAt.After(t.LastDetectedAt) {
		t.LastDetectedAt = d.DetectedAt
	}
	if len(t.Days) == 0 {
		s.Species = slices.Delete(s.Species, i, i+1)
	}
}
