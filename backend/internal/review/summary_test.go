package review

import (
	"maps"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

const card = "OWL-20260914-SR02"

func newStore(t *testing.T) db.Store {
	t.Helper()
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// heard is a BirdNET detection on the card, at an instant, in a status.
func heard(id, scientific, common string, at time.Time, status string) db.Detection {
	return db.Detection{
		ID: id, AudioFileID: "af_1", DetectedAt: at, ScientificName: scientific, CommonName: common,
		Confidence: 0.8, ReviewStatus: status,
	}
}

var (
	// 23:30 on Sep 12 in Seattle, which is already Sep 13 in UTC.
	lateEvening = time.Date(2026, time.September, 13, 6, 30, 0, 0, time.UTC)
	nextMorning = time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
)

func tallies(s db.CardSummary) map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, t := range s.Species {
		out[t.ScientificName] = t.Days
	}
	return out
}

func TestBuildCountsWhatCounts(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	crow := heard("det_crow", "Corvus brachyrhynchos", "American Crow", lateEvening, db.ReviewAccepted)
	owl := heard("det_owl", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewConfirmed)
	// Confirmed as what the reviewer said it was.
	corrected := heard("det_corrected", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewConfirmed)
	corrected.Review = &db.Review{CorrectedScientificName: "Strix varia", CorrectedCommonName: "Barred Owl"}
	perch := heard("det_perch", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewConfirmed)
	perch.Model = db.ModelPerch
	if err := store.UpsertDetections(ctx, card, []db.Detection{
		crow, owl, corrected, perch,
		heard("det_unreviewed", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewUnreviewed),
		heard("det_rejected", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewRejected),
	}); err != nil {
		t.Fatal(err)
	}

	built, err := Build(ctx, store, card)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"Bubo virginianus":      {"2026-09-13": 1},
		"Corvus brachyrhynchos": {"2026-09-12": 1}, // the Pacific date, not UTC's
		"Strix varia":           {"2026-09-13": 1},
	}
	if got := tallies(built); !maps.EqualFunc(got, want, maps.Equal) || built.Detections != 3 {
		t.Errorf("summary = %v, %d detections; want %v, 3: BirdNET's accepted and confirmed, corrections as corrected", got, built.Detections, want)
	}
	if built.Species[0].ScientificName != "Bubo virginianus" || built.Species[2].ScientificName != "Strix varia" {
		t.Errorf("species in order %v; want them sorted by scientific name", built.Species)
	}
	if !built.Species[1].LastDetectedAt.Equal(lateEvening) {
		t.Errorf("the crow was last heard %v, want %v", built.Species[1].LastDetectedAt, lateEvening)
	}

	stored, err := store.ListCardSummaries(ctx)
	if err != nil || len(stored) != 1 || !maps.EqualFunc(tallies(stored[0]), want, maps.Equal) {
		t.Errorf("stored summaries = %+v (%v); want the one built", stored, err)
	}
}

func TestAdjustFollowsEachChange(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	owl := heard("det_owl", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewUnreviewed)
	if err := store.UpsertDetections(ctx, card, []db.Detection{owl}); err != nil {
		t.Fatal(err)
	}

	// Before anything has built it, a change leaves the card without one: the
	// first read builds it whole, and that includes the change.
	confirmed := owl
	confirmed.ReviewStatus = db.ReviewConfirmed
	if err := Adjust(ctx, store, card, []db.Detection{owl}, []db.Detection{confirmed}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := store.ListCardSummaries(ctx); len(stored) != 0 {
		t.Fatalf("a change made a summary: %+v", stored)
	}

	if _, err := Build(ctx, store, card); err != nil {
		t.Fatal(err)
	}
	summary := func() db.CardSummary {
		t.Helper()
		stored, err := store.ListCardSummaries(ctx)
		if err != nil || len(stored) != 1 {
			t.Fatalf("summaries = %+v, %v", stored, err)
		}
		return stored[0]
	}

	// Confirming it is one more; correcting it moves it to another species.
	other := heard("det_other", "Bubo virginianus", "Great Horned Owl", lateEvening, db.ReviewUnreviewed)
	otherConfirmed := other
	otherConfirmed.ReviewStatus = db.ReviewConfirmed
	if err := Adjust(ctx, store, card, []db.Detection{other}, []db.Detection{otherConfirmed}); err != nil {
		t.Fatal(err)
	}
	corrected := confirmed
	corrected.Review = &db.Review{CorrectedScientificName: "Strix varia", CorrectedCommonName: "Barred Owl"}
	if err := Adjust(ctx, store, card, []db.Detection{confirmed}, []db.Detection{corrected}); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"Bubo virginianus": {"2026-09-12": 1},
		"Strix varia":      {"2026-09-13": 1},
	}
	if s := summary(); !maps.EqualFunc(tallies(s), want, maps.Equal) || s.Detections != 2 {
		t.Errorf("after confirming and correcting: %v, %d; want %v, 2", tallies(s), s.Detections, want)
	}

	// Taking both back leaves nothing, not empty species.
	if err := Adjust(ctx, store, card, []db.Detection{corrected, otherConfirmed}, []db.Detection{owl, other}); err != nil {
		t.Fatal(err)
	}
	if s := summary(); len(s.Species) != 0 || s.Detections != 0 {
		t.Errorf("after undoing both: %+v; want nothing counted", s)
	}

	// A change that counts the same as before doesn't write.
	before := summary().UpdatedAt
	if err := Adjust(ctx, store, card, []db.Detection{owl}, []db.Detection{owl}); err != nil {
		t.Fatal(err)
	}
	if after := summary().UpdatedAt; !after.Equal(before) {
		t.Errorf("an unchanged count rewrote the summary (%v, then %v)", before, after)
	}
}
