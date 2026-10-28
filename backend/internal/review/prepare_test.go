package review

import (
	"errors"
	"io"
	"log/slog"
	"slices"
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

// Preparing a card bouts what goes to review and picks spot-checks from what
// was accepted; a bout someone has heard stays as it is through a rules
// change, and the rest follow the rules.
func TestPreparingMakesBouts(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	minute := func(m int) time.Time { return nextMorning.Add(time.Duration(m) * time.Minute) }
	at := func(id, species, common string, m int, confidence float64) db.Detection {
		d := heard(id, species, common, minute(m), db.ReviewUnreviewed)
		d.Confidence = confidence
		return d
	}
	detections := []db.Detection{
		// Two owl bouts at a 15-minute gap.
		at("owl1", "Bubo virginianus", "Great Horned Owl", 0, 0.4),
		at("owl2", "Bubo virginianus", "Great Horned Owl", 10, 0.9),
		at("owl3", "Bubo virginianus", "Great Horned Owl", 60, 0.5),
		// Three crow bouts, accepted: one is picked to check.
		at("crow1", "Corvus brachyrhynchos", "American Crow", 0, 0.8),
		at("crow2", "Corvus brachyrhynchos", "American Crow", 60, 0.8),
		at("crow3", "Corvus brachyrhynchos", "American Crow", 120, 0.8),
		// Below the crows' floor: in no bout.
		at("faint", "Corvus brachyrhynchos", "American Crow", 1, 0.2),
	}
	// Reviewed on its own page before bouts: left out of them.
	chickadee := at("chickadee", "Poecile gambeli", "Mountain Chickadee", 5, 0.7)
	chickadee.ReviewStatus = db.ReviewRejected
	chickadee.Review = &db.Review{UserID: "usr_1", UserName: "Ellen Park"}
	analyzedCard(t, store, card, db.StatusInReview, append(detections, chickadee)...)
	rules := testRules()
	rules.Version, rules.SpotChecksPerSpecies = 0, 1
	saved, err := store.SaveReviewRules(ctx, rules)
	if err != nil {
		t.Fatal(err)
	}
	p := newPreparer(store)
	if again := p.Pass(ctx); again != 0 {
		t.Fatalf("pass failed, again in %v", again)
	}

	bouts := boutsOf(t, store)
	if len(bouts) != 3 {
		t.Fatalf("made %d bouts, want 2 of owls and 1 crow spot-check: %+v", len(bouts), bouts)
	}
	owlBout, laterOwl := bouts[db.BoutID(card, "Bubo virginianus", "owl1")], bouts[db.BoutID(card, "Bubo virginianus", "owl3")]
	if owlBout.Kind != db.BoutReview || !slices.Equal(owlBout.DetectionIDs, []string{"owl1", "owl2"}) || laterOwl.Kind != db.BoutReview {
		t.Errorf("owl bouts = %+v, %+v", owlBout, laterOwl)
	}
	var check db.Bout
	for _, b := range bouts {
		if b.Kind == db.BoutSpotCheck {
			check = b
		}
	}
	if check.ScientificName != "Corvus brachyrhynchos" || len(check.DetectionIDs) != 1 {
		t.Errorf("spot-check = %+v, want one crow bout", check)
	}
	inBout := map[string]string{}
	for _, b := range bouts {
		for _, id := range b.DetectionIDs {
			inBout[id] = b.ID
		}
	}
	for _, d := range append(detections, chickadee) {
		got, err := store.GetDetection(ctx, card, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.BoutID != inBout[d.ID] {
			t.Errorf("%s is in bout %q, want %q", d.ID, got.BoutID, inBout[d.ID])
		}
	}
	if u, _ := store.GetUpload(ctx, card); u.Review == nil || u.Review.Bouts != 3 || u.Review.Resolved != 0 {
		t.Errorf("card review = %+v, want 3 bouts", u.Review)
	}

	// Someone hears the first owl bout; then the rules accept owls.
	if _, err := store.UpdateBout(ctx, card, owlBout.ID, func(b *db.Bout) error {
		b.Reviews = append(b.Reviews, db.BoutVerdict{UserID: "usr_1", UserName: "Ellen Park", Verdict: "confirm"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved.Species[1].Action = db.RouteAccept
	if _, err := store.SaveReviewRules(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if again := p.Pass(ctx); again != 0 {
		t.Fatalf("pass failed, again in %v", again)
	}
	bouts = boutsOf(t, store)
	if b := bouts[owlBout.ID]; len(b.Reviews) != 1 || b.Kind != db.BoutReview || !slices.Equal(b.DetectionIDs, owlBout.DetectionIDs) {
		t.Errorf("the heard bout became %+v", b)
	}
	for _, id := range []string{"owl1", "owl2"} {
		if d, _ := store.GetDetection(ctx, card, id); d.Route != db.RouteReview || d.ReviewStatus != db.ReviewUnreviewed || d.BoutID != owlBout.ID {
			t.Errorf("%s in the heard bout: %q, %s, in %q; want it as it was", id, d.Route, d.ReviewStatus, d.BoutID)
		}
	}
	// The later owl is accepted now, and the only owl bout to check.
	if b := bouts[laterOwl.ID]; b.Kind != db.BoutSpotCheck {
		t.Errorf("the later owl's bout = %+v, want it a spot-check now", b)
	}
	if d, _ := store.GetDetection(ctx, card, "owl3"); d.Route != db.RouteAccept || d.ReviewStatus != db.ReviewAccepted || d.BoutID != laterOwl.ID {
		t.Errorf("owl3: %q, %s, in %q; want accepted, checked", d.Route, d.ReviewStatus, d.BoutID)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.Bouts != 3 {
		t.Errorf("card review = %+v, want 3 bouts", u.Review)
	}
}

// A bout heard between preparing's read and its write is left alone, and
// the card is prepared again rather than recorded as prepared.
func TestPreparingLeavesABoutHeardMeanwhile(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	b := db.Bout{ID: "bout_1", UploadID: card, Kind: db.BoutReview, ScientificName: "Bubo virginianus", DetectionIDs: []string{"owl1"}}
	if _, err := store.CreateBout(ctx, b); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ListBouts(ctx, card)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBout(ctx, card, b.ID, func(b *db.Bout) error {
		b.Reviews = append(b.Reviews, db.BoutVerdict{UserID: "usr_1", Verdict: "reject"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := newPreparer(store)
	changed := b
	changed.DetectionIDs = []string{"owl1", "owl2"}
	for name, made := range map[string][]db.Bout{"rewritten": {changed}, "deleted": nil} {
		if _, _, err := p.writeBouts(ctx, card, stale, made); !errors.Is(err, errHeardMeanwhile) {
			t.Errorf("%s: %v, want errHeardMeanwhile", name, err)
		}
		if got := boutsOf(t, store)[b.ID]; len(got.Reviews) != 1 || len(got.DetectionIDs) != 1 {
			t.Errorf("%s: the heard bout became %+v", name, got)
		}
	}
}

func boutsOf(t *testing.T, store db.Store) map[string]db.Bout {
	t.Helper()
	bouts, err := store.ListBouts(t.Context(), card)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]db.Bout{}
	for _, b := range bouts {
		out[b.ID] = b
	}
	return out
}

// A card prepared by routing alone, before there were bouts, is prepared
// again for them, though the rules haven't changed.
func TestACardPreparedBeforeBoutsIsPreparedAgain(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	owl := heard("owl1", "Bubo virginianus", "Great Horned Owl", nextMorning, db.ReviewUnreviewed)
	owl.Route = db.RouteReview
	analyzedCard(t, store, card, db.StatusInReview, owl)
	rules := testRules()
	rules.Version = 0
	saved, err := store.SaveReviewRules(ctx, rules)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUpload(ctx, card, func(u *db.Upload) error {
		u.Review = &db.UploadReview{RulesVersion: saved.Version, PreparedAt: nextMorning}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newPreparer(store).Pass(ctx)
	if d, _ := store.GetDetection(ctx, card, "owl1"); d.BoutID == "" {
		t.Error("the owl was left out of a bout")
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.Preparation != Preparation || u.Review.Bouts != 1 {
		t.Errorf("card review = %+v, want prepared with bouts", u.Review)
	}
}
