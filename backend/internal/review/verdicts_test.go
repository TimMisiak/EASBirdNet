package review

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
)

func verdict(user, v string, species ...string) db.BoutVerdict {
	out := db.BoutVerdict{UserID: user, UserName: user, Verdict: v}
	if len(species) > 0 {
		out.ScientificName = species[0]
	}
	return out
}

func TestHowABoutSettles(t *testing.T) {
	const barred = "Strix varia"
	for _, c := range []struct {
		name     string
		kind     string
		verdicts []db.BoutVerdict
		want     string
	}{
		{"one confirm", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm")}, db.BoutOpen},
		{"two confirm", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "confirm")}, db.BoutConfirmed},
		{"two reject", db.BoutReview, []db.BoutVerdict{verdict("a", "reject"), verdict("b", "reject")}, db.BoutRejected},
		{"split", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "reject")}, db.BoutOpen},
		{"the third settles it", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "reject"), verdict("c", "reject")}, db.BoutRejected},
		{"two species", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "confirm", barred)}, db.BoutOpen},
		{"both another species", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm", barred), verdict("b", "confirm", barred)}, db.BoutConfirmed},
		{"three apart", db.BoutReview, []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "confirm", barred), verdict("c", "reject")}, db.BoutDisputed},
		{"unsure matches nothing", db.BoutReview, []db.BoutVerdict{verdict("a", "unsure"), verdict("b", "unsure"), verdict("c", "unsure")}, db.BoutDisputed},
		{"a spot-check confirmed once", db.BoutSpotCheck, []db.BoutVerdict{verdict("a", "confirm")}, db.BoutConfirmed},
		{"a spot-check as another species", db.BoutSpotCheck, []db.BoutVerdict{verdict("a", "confirm", barred)}, db.BoutOpen},
		{"a spot-check rejected once", db.BoutSpotCheck, []db.BoutVerdict{verdict("a", "reject")}, db.BoutOpen},
		{"a spot-check rejected twice", db.BoutSpotCheck, []db.BoutVerdict{verdict("a", "reject"), verdict("b", "reject")}, db.BoutRejected},
		{"a spot-check in doubt", db.BoutSpotCheck, []db.BoutVerdict{verdict("a", "unsure"), verdict("b", "confirm")}, db.BoutOpen},
	} {
		b := db.Bout{ID: "bout_1", Kind: c.kind, ScientificName: "Bubo virginianus", Status: db.BoutOpen}
		for i, v := range c.verdicts {
			v.At = nextMorning.Add(time.Duration(i) * time.Minute)
			if err := Give(&b, v); err != nil && i == len(c.verdicts)-1 {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		if b.Status != c.want {
			t.Errorf("%s: %s, want %s", c.name, b.Status, c.want)
		}
	}
}

func TestGivingAVerdict(t *testing.T) {
	b := db.Bout{ID: "bout_1", Kind: db.BoutReview, ScientificName: "Bubo virginianus", Status: db.BoutOpen}
	// Naming the bout's own species is confirming it.
	if err := Give(&b, verdict("a", "confirm", "Bubo virginianus")); err != nil || b.Reviews[0].ScientificName != "" {
		t.Fatalf("own species: %v, %+v", err, b.Reviews)
	}
	// A listener changes their mind: one verdict each.
	if err := Give(&b, verdict("a", "reject")); err != nil || len(b.Reviews) != 1 || b.Reviews[0].Verdict != "reject" {
		t.Fatalf("changed: %v, %+v", err, b.Reviews)
	}
	if err := Give(&b, verdict("b", "reject")); err != nil || b.Status != db.BoutRejected {
		t.Fatalf("second reject: %v, %s", err, b.Status)
	}
	// Settled: the same again is harmless, anything else refused.
	if err := Give(&b, verdict("b", "reject")); err != nil {
		t.Errorf("the same verdict again: %v", err)
	}
	if err := Give(&b, verdict("b", "confirm")); !errors.Is(err, ErrSettled) {
		t.Errorf("a change once settled: %v, want ErrSettled", err)
	}
	if err := Give(&b, verdict("c", "confirm")); !errors.Is(err, ErrSettled) {
		t.Errorf("a third listener once settled: %v, want ErrSettled", err)
	}
	disputed := db.Bout{Kind: db.BoutReview, Status: db.BoutDisputed}
	if err := Give(&disputed, verdict("d", "confirm")); !errors.Is(err, ErrDisputed) {
		t.Errorf("a verdict on a disputed bout: %v, want ErrDisputed", err)
	}
}

func TestTheFinalSay(t *testing.T) {
	b := db.Bout{Kind: db.BoutReview, ScientificName: "Bubo virginianus", Status: db.BoutOpen}
	for _, v := range []db.BoutVerdict{verdict("a", "confirm"), verdict("b", "reject"), verdict("c", "unsure")} {
		if err := Give(&b, v); err != nil {
			t.Fatal(err)
		}
	}
	if b.Status != db.BoutDisputed {
		t.Fatalf("status %s, want disputed", b.Status)
	}
	if err := Decide(&b, verdict("e", "unsure")); !errors.Is(err, ErrNotAFinalSay) {
		t.Errorf("an unsure final say: %v", err)
	}
	if err := Decide(&b, verdict("e", "confirm", "Bubo virginianus")); err != nil || b.Status != db.BoutConfirmed || b.Resolution.ScientificName != "" {
		t.Errorf("final say: %v, %s, %+v; want confirmed, of its own species", err, b.Status, b.Resolution)
	}
	if s, ok := Settled(b); !ok || len(s.By) != 1 || s.By[0].UserID != "e" {
		t.Errorf("settled by %+v, want the expert alone", s.By)
	}
	// An override of a settled bout.
	if err := Decide(&b, verdict("e", "reject")); err != nil || b.Status != db.BoutRejected {
		t.Errorf("override: %v, %s; want rejected", err, b.Status)
	}
}

// reviewedCard stores a prepared card with a bout of three owl detections
// to review and its summary, under testRules, and answers the bout.
func reviewedCard(t *testing.T, store db.Store) db.Bout {
	t.Helper()
	ctx := t.Context()
	var ids []string
	var detections []db.Detection
	for i := range 3 {
		d := heard("owl"+string(rune('1'+i)), "Bubo virginianus", "Great Horned Owl", nextMorning.Add(time.Duration(i)*time.Minute), db.ReviewUnreviewed)
		d.Route = db.RouteReview
		ids = append(ids, d.ID)
		detections = append(detections, d)
	}
	analyzedCard(t, store, card, db.StatusInReview, detections...)
	rules := testRules()
	rules.Version = 0
	if _, err := store.SaveReviewRules(ctx, rules); err != nil {
		t.Fatal(err)
	}
	if err := newPreparer(store).Prepare(ctx, NewTable(&rules), card); err != nil {
		t.Fatal(err)
	}
	b, err := store.GetBout(ctx, card, db.BoutID(card, "Bubo virginianus", ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// give stores a verdict as the API does, and carries it out.
func give(t *testing.T, store db.Store, b db.Bout, v db.BoutVerdict) db.Bout {
	t.Helper()
	var was string
	b, err := store.UpdateBout(t.Context(), card, b.ID, func(b *db.Bout) error {
		was = b.Status
		return Give(b, v)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Resolve(t.Context(), store, was, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestASettledBoutsVerdictReachesItsDetections(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	b := reviewedCard(t, store)
	// One of them a reviewer confirmed on its own page: that stands.
	if _, err := store.UpdateDetection(ctx, card, "owl3", func(d *db.Detection) error {
		d.ReviewStatus, d.Review = db.ReviewRejected, &db.Review{UserID: "usr_9", UserName: "Sam Lee"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	b = give(t, store, b, db.BoutVerdict{UserID: "usr_1", UserName: "Ellen Park", Verdict: "confirm", ScientificName: "Strix varia", CommonName: "Barred Owl", At: nextMorning})
	if d, _ := store.GetDetection(ctx, card, "owl1"); d.ReviewStatus != db.ReviewUnreviewed {
		t.Fatalf("one verdict reached the detections: %s", d.ReviewStatus)
	}
	b = give(t, store, b, db.BoutVerdict{UserID: "usr_2", UserName: "Raj Patel", Verdict: "confirm", ScientificName: "Strix varia", CommonName: "Barred Owl", At: nextMorning.Add(time.Minute)})
	if b.Status != db.BoutConfirmed {
		t.Fatalf("bout is %s, want confirmed", b.Status)
	}
	for _, id := range []string{"owl1", "owl2"} {
		d, _ := store.GetDetection(ctx, card, id)
		r := d.Review
		if d.ReviewStatus != db.ReviewConfirmed || r == nil || r.BoutID != b.ID || r.UserName != "Raj Patel" ||
			!slices.Equal(r.AgreedBy, []string{"Ellen Park", "Raj Patel"}) || r.CorrectedScientificName != "Strix varia" || r.CorrectedCommonName != "Barred Owl" {
			t.Errorf("%s: %s, %+v; want confirmed as a Barred Owl by both", id, d.ReviewStatus, r)
		}
	}
	if d, _ := store.GetDetection(ctx, card, "owl3"); d.ReviewStatus != db.ReviewRejected || d.Review.UserName != "Sam Lee" {
		t.Errorf("the detection reviewed on its own: %s, %+v; want it as its reviewer left it", d.ReviewStatus, d.Review)
	}
	if got := tallies(summaryOf(t, store, card)); got["Strix varia"]["2026-09-13"] != 2 || len(got) != 1 {
		t.Errorf("summary = %v, want the two Barred Owls", got)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.Bouts != 1 || u.Review.Resolved != 1 {
		t.Errorf("card review = %+v, want its one bout resolved", u.Review)
	}
	rules, _ := store.GetReviewRules(ctx)
	i := slices.IndexFunc(rules.Species, func(r db.SpeciesRule) bool { return r.ScientificName == "Strix varia" })
	if i < 0 || rules.Species[i].Source != db.RuleSourceReview || rules.Species[i].Action != db.RouteReview ||
		rules.Species[i].MinConfidence != 0.5 || rules.Species[i].AddedIn != rules.Version || rules.Saved() != rules.Version-1 {
		t.Errorf("rules = %+v; want Barred Owl added to review, as a version of its own", rules)
	}

	// The same verdict again changes nothing, and adds no second row.
	again := give(t, store, b, db.BoutVerdict{UserID: "usr_2", UserName: "Raj Patel", Verdict: "confirm", ScientificName: "Strix varia", At: nextMorning.Add(time.Hour)})
	if again.Status != db.BoutConfirmed {
		t.Errorf("bout is %s after the verdict again", again.Status)
	}
	if after, _ := store.GetReviewRules(ctx); after.Version != rules.Version {
		t.Errorf("rules went to version %d", after.Version)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.Resolved != 1 {
		t.Errorf("card review = %+v after the verdict again", u.Review)
	}
}

// A verdict whose copying was cut short is finished by preparing the card
// again, and it is a spot-check's rejection that turns accepted detections
// rejected.
func TestPreparingFinishesASettledBout(t *testing.T) {
	store := newStore(t)
	ctx := t.Context()
	crow := heard("crow1", "Corvus brachyrhynchos", "American Crow", nextMorning, db.ReviewUnreviewed)
	analyzedCard(t, store, card, db.StatusInReview, crow)
	rules := testRules()
	rules.Version = 0
	saved, err := store.SaveReviewRules(ctx, rules)
	if err != nil {
		t.Fatal(err)
	}
	p := newPreparer(store)
	p.Pass(ctx)
	if s := summaryOf(t, store, card); s.Detections != 1 {
		t.Fatalf("summary counts %d, want the accepted crow", s.Detections)
	}
	b, err := store.GetBout(ctx, card, db.BoutID(card, "Corvus brachyrhynchos", "crow1"))
	if err != nil || b.Kind != db.BoutSpotCheck {
		t.Fatalf("spot-check: %+v, %v", b, err)
	}
	// Two rejections stored, and nothing after: as if the server stopped.
	for _, user := range []string{"usr_1", "usr_2"} {
		if _, err := store.UpdateBout(ctx, card, b.ID, func(b *db.Bout) error {
			return Give(b, db.BoutVerdict{UserID: user, UserName: user, Verdict: "reject"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := Unprepare(ctx, store, card); err != nil {
		t.Fatal(err)
	}
	if again := p.Pass(ctx); again != 0 {
		t.Fatalf("pass failed, again in %v", again)
	}
	d, _ := store.GetDetection(ctx, card, "crow1")
	if d.ReviewStatus != db.ReviewRejected || d.Review == nil || d.Review.BoutID != b.ID || d.Route != db.RouteAccept {
		t.Errorf("crow: %s, %+v; want rejected by its spot-check", d.ReviewStatus, d.Review)
	}
	if s := summaryOf(t, store, card); s.Detections != 0 {
		t.Errorf("summary counts %d, want the rejected crow gone", s.Detections)
	}
	if u, _ := store.GetUpload(ctx, card); u.Review.Preparation != Preparation || u.Review.Resolved != 1 || u.Review.RulesVersion != saved.Version {
		t.Errorf("card review = %+v, want prepared, its bout resolved", u.Review)
	}
}
