package review

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

func newPreparer(store db.Store) *Preparer {
	return NewPreparer(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// analyzedCard stores a card BirdNET has finished with, in status, holding
// detections stored before there were rules.
func analyzedCard(t *testing.T, store db.Store, id, status string, detections ...db.Detection) {
	t.Helper()
	ctx := t.Context()
	if _, err := store.CreateUpload(ctx, db.Upload{ID: id, Status: status, FileCount: 2, FilesAnalyzed: 1, FilesFailed: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDetections(ctx, id, detections); err != nil {
		t.Fatal(err)
	}
}

func TestPreparingRoutesACardAnalyzedBeforeTheRules(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	crow := heard("det_crow", "Corvus brachyrhynchos", "American Crow", nextMorning, db.ReviewUnreviewed)
	crow.Confidence = 0.9
	faint := heard("det_faint", "Corvus brachyrhynchos", "American Crow", nextMorning, db.ReviewUnreviewed)
	faint.Confidence = 0.3
	owl := heard("det_owl", "Bubo virginianus", "Great Horned Owl", lateEvening, db.ReviewUnreviewed)
	// A reviewer discarded this crow before there were rules; that stands.
	discarded := heard("det_discarded", "Corvus brachyrhynchos", "American Crow", nextMorning, db.ReviewRejected)
	discarded.Confidence = 0.9
	discarded.Review = &db.Review{UserID: "usr_1", UserName: "Ellen Park"}
	analyzedCard(t, store, card, db.StatusInReview, crow, faint, owl, discarded)
	// Still arriving, and still being analyzed: neither is ready.
	analyzedCard(t, store, "OWL-20260914-SR03", db.StatusInProgress, heard("det_a", "Corvus brachyrhynchos", "American Crow", nextMorning, db.ReviewUnreviewed))
	if _, err := store.CreateUpload(ctx, db.Upload{ID: "OWL-20260914-SR04", Status: db.StatusProcessing, FileCount: 2, FilesAnalyzed: 1}); err != nil {
		t.Fatal(err)
	}

	p := newPreparer(store)
	// With no rules there is nothing to route by.
	if again := p.Pass(ctx); again != 0 {
		t.Fatalf("a pass with no rules asked to come back in %v", again)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review != nil {
		t.Fatalf("prepared with no rules: %+v", u.Review)
	}

	rules := testRules()
	rules.Version = 0
	saved, err := store.SaveReviewRules(ctx, rules)
	if err != nil {
		t.Fatal(err)
	}
	if again := p.Pass(ctx); again != 0 {
		t.Fatalf("pass failed, again in %v", again)
	}

	want := map[string][2]string{
		"det_crow":      {db.RouteAccept, db.ReviewAccepted},
		"det_faint":     {db.RouteIgnore, db.ReviewUnreviewed},
		"det_owl":       {db.RouteReview, db.ReviewUnreviewed},
		"det_discarded": {db.RouteAccept, db.ReviewRejected},
	}
	for id, w := range want {
		d, err := store.GetDetection(ctx, card, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Route != w[0] || d.ReviewStatus != w[1] {
			t.Errorf("%s: %q, %s; want %q, %s", id, d.Route, d.ReviewStatus, w[0], w[1])
		}
	}
	if d, _ := store.GetDetection(ctx, "OWL-20260914-SR03", "det_a"); d.Route != "" {
		t.Errorf("a card still arriving was routed: %+v", d)
	}
	u, _ := store.GetUpload(ctx, card)
	if u.Review == nil || u.Review.RulesVersion != saved.Version {
		t.Errorf("card review = %+v, want prepared by version %d", u.Review, saved.Version)
	}
	if s := summaryOf(t, store, card); s.Detections != 1 {
		t.Errorf("summary counts %d, want the accepted crow", s.Detections)
	}

	// Prepared by these rules, the card is left alone by the next pass.
	before := u.Review.PreparedAt
	p.now = func() time.Time { return before.Add(time.Hour) }
	p.Pass(ctx)
	if u, _ := store.GetUpload(ctx, card); !u.Review.PreparedAt.Equal(before) {
		t.Errorf("prepared again under the same rules, at %v", u.Review.PreparedAt)
	}

	// Rules that stop accepting crows send the accepted one to review.
	saved.Species[0].Action = db.RouteReview
	if saved, err = store.SaveReviewRules(ctx, saved); err != nil {
		t.Fatal(err)
	}
	p.Pass(ctx)
	if d, _ := store.GetDetection(ctx, card, "det_crow"); d.Route != db.RouteReview || d.ReviewStatus != db.ReviewUnreviewed {
		t.Errorf("crow after the rules changed: %q, %s; want sent to review", d.Route, d.ReviewStatus)
	}
	if s := summaryOf(t, store, card); s.Detections != 0 {
		t.Errorf("summary counts %d after the change, want nothing", s.Detections)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.RulesVersion != saved.Version {
		t.Errorf("card prepared by version %d, want %d", u.Review.RulesVersion, saved.Version)
	}
}

func summaryOf(t *testing.T, store db.Store, id string) db.CardSummary {
	t.Helper()
	all, err := store.ListCardSummaries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("%s has no summary", id)
	return db.CardSummary{}
}
