package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/review"
)

type boutsBody struct {
	Card  ReviewCard   `json:"card"`
	Bouts []ReviewBout `json:"bouts"`
	Left  int          `json:"left"`
}

type boutBody struct {
	Bout       ReviewBout  `json:"bout"`
	Detections []Detection `json:"detections"`
}

// reviewFixture is a prepared card with one bout of two owl detections to
// review, under rules that have no row for owls, and the people to hear it.
func reviewFixture(t *testing.T) (*http.ServeMux, db.Store, db.Bout) {
	t.Helper()
	mux, store := newTestMux(t)
	ctx := t.Context()
	for _, u := range []db.User{
		{Name: "Raj Reviewer", Email: "raj@example.com", Role: db.RoleVolunteer, Roles: []string{db.RoleReviewer}},
		{Name: "Eve Expert", Email: "eve@example.com", Role: db.RoleVolunteer, Roles: []string{db.RoleBirdingExpert}},
	} {
		if _, err := store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SaveReviewRules(ctx, db.ReviewRules{
		Defaults: db.RuleAction{Action: db.RouteReview, MinConfidence: 0.5}, BoutGapMinutes: 15, SpotChecksPerSpecies: 3,
	}); err != nil {
		t.Fatal(err)
	}
	const ref = "OWL-20260920-SR09"
	if _, err := store.CreateUpload(ctx, db.Upload{
		ID: ref, RecorderID: "SR-09", Recorder: db.RecorderSnapshot{Name: "Marymoor – Snag Row"}, Status: db.StatusInReview,
		Review: &db.UploadReview{RulesVersion: 1, Preparation: review.Preparation, Bouts: 1},
	}); err != nil {
		t.Fatal(err)
	}
	b := db.Bout{
		ID: "bout_owls", UploadID: ref, RecorderID: "SR-09", Kind: db.BoutReview,
		ScientificName: "Bubo virginianus", CommonName: "Great Horned Owl",
		FirstHeardAt: testNow.Add(-2 * time.Hour), LastHeardAt: testNow.Add(-time.Hour),
		DetectionIDs: []string{"det_owl1", "det_owl2"}, Best: []string{"det_owl2", "det_owl1"}, MaxConfidence: 0.9,
	}
	var detections []db.Detection
	for i, id := range b.DetectionIDs {
		detections = append(detections, db.Detection{
			ID: id, AudioFileID: "af_1", DetectedAt: b.FirstHeardAt.Add(time.Duration(i) * time.Hour),
			ScientificName: b.ScientificName, CommonName: b.CommonName, Confidence: 0.6 + 0.3*float64(i),
			Route: db.RouteReview, BoutID: b.ID, ReviewStatus: db.ReviewUnreviewed,
		})
	}
	if err := store.UpsertDetections(ctx, ref, detections); err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateBout(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	return mux, store, b
}

// Reviewers hear a bout blind; two that agree settle it, and the verdict
// goes to its detections.
func TestReviewingABout(t *testing.T) {
	mux, store, b := reviewFixture(t)
	ctx := t.Context()
	jane := signInAs(t, mux, "jane@example.com")
	raj := signInAs(t, mux, "raj@example.com")
	eve := signInAs(t, mux, "eve@example.com")
	marcus := signInAs(t, mux, "m.lee@example.com")
	const cards = "/api/v1/review/cards"
	bouts := cards + "/" + b.UploadID + "/bouts"
	verdict := bouts + "/" + b.ID + "/verdict"

	if rec := do(t, mux, http.MethodGet, cards, "", marcus); rec.Code != http.StatusForbidden {
		t.Errorf("a field volunteer GET cards = %d, want 403", rec.Code)
	}
	rec := do(t, mux, http.MethodGet, cards, "", jane)
	got := decodeInto[struct {
		Cards []ReviewCard `json:"cards"`
	}](t, rec)
	if rec.Code != http.StatusOK || len(got.Cards) != 1 || got.Cards[0].Reference != b.UploadID || got.Cards[0].Bouts != 1 ||
		got.Cards[0].StationName != "Marymoor – Snag Row" {
		t.Fatalf("GET cards = %d %s; want the one card with bouts", rec.Code, rec.Body)
	}
	rec = do(t, mux, http.MethodGet, bouts, "", jane)
	list := decodeInto[boutsBody](t, rec)
	if rec.Code != http.StatusOK || list.Left != 1 || len(list.Bouts) != 1 || !list.Bouts[0].ForMe || list.Bouts[0].Detections != 2 {
		t.Fatalf("GET bouts = %d %s; want the one bout, waiting for Jane", rec.Code, rec.Body)
	}

	for _, bad := range []string{`{"verdict":"maybe"}`, `{"verdict":"reject","scientificName":"Strix varia"}`, `{"verdict":"confirm","commonName":"Barred Owl"}`} {
		if rec := do(t, mux, http.MethodPut, verdict, bad, jane); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", bad, rec.Code)
		}
	}
	if rec := do(t, mux, http.MethodPut, bouts+"/bout_nope/verdict", `{"verdict":"confirm"}`, jane); rec.Code != http.StatusNotFound {
		t.Errorf("PUT a verdict on no bout = %d, want 404", rec.Code)
	}

	rec = do(t, mux, http.MethodPut, verdict, `{"verdict":"confirm"}`, jane)
	mine := decodeInto[boutBody](t, rec).Bout
	if rec.Code != http.StatusOK || mine.Status != db.BoutOpen || mine.Mine == nil || mine.Mine.Verdict != "confirm" || len(mine.Verdicts) != 1 || mine.ForMe {
		t.Fatalf("Jane's verdict = %d %s; want it hers, the bout still open", rec.Code, rec.Body)
	}

	// Raj hasn't given one: he doesn't see Jane's. The expert does.
	rec = do(t, mux, http.MethodGet, bouts+"/"+b.ID, "", raj)
	one := decodeInto[boutBody](t, rec)
	if rec.Code != http.StatusOK || one.Bout.Listeners != 1 || one.Bout.Verdicts != nil || !one.Bout.ForMe || len(one.Detections) != 2 {
		t.Fatalf("GET the bout as Raj = %d %s; want one listener, verdicts hidden, its two detections", rec.Code, rec.Body)
	}
	if one := decodeInto[boutBody](t, do(t, mux, http.MethodGet, bouts+"/"+b.ID, "", eve)); len(one.Bout.Verdicts) != 1 || one.Bout.Verdicts[0].By != "Jane Volunteer" {
		t.Errorf("the expert sees verdicts %+v, want Jane's", one.Bout.Verdicts)
	}

	// Raj disagrees, then changes his mind: two confirms settle it.
	if rec := do(t, mux, http.MethodPut, verdict, `{"verdict":"reject"}`, raj); decodeInto[boutBody](t, rec).Bout.Status != db.BoutOpen {
		t.Fatalf("a split = %s; want the bout still open", rec.Body)
	}
	rec = do(t, mux, http.MethodPut, verdict, `{"verdict":"confirm"}`, raj)
	if settled := decodeInto[boutBody](t, rec).Bout; rec.Code != http.StatusOK || settled.Status != db.BoutConfirmed || settled.Listeners != 2 {
		t.Fatalf("two confirms = %d %s; want the bout confirmed", rec.Code, rec.Body)
	}
	for _, d := range decodeInto[boutBody](t, do(t, mux, http.MethodGet, bouts+"/"+b.ID, "", jane)).Detections {
		if d.ReviewStatus != db.ReviewConfirmed || d.Review == nil || d.Review.By != "Jane Volunteer, Raj Reviewer" || d.Review.BoutID != b.ID {
			t.Errorf("%s: %s, %+v; want confirmed by both, in the bout", d.ID, d.ReviewStatus, d.Review)
		}
	}
	if rec := do(t, mux, http.MethodPut, verdict, `{"verdict":"reject"}`, eve); rec.Code != http.StatusConflict {
		t.Errorf("a verdict on a settled bout = %d, want 409", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, verdict, `{"verdict":"confirm"}`, raj); rec.Code != http.StatusOK {
		t.Errorf("the same verdict again = %d %s, want 200", rec.Code, rec.Body)
	}
	if u, _ := store.GetUpload(ctx, b.UploadID); u.Review.Resolved != 1 {
		t.Errorf("card review = %+v, want its bout resolved", u.Review)
	}
	rules, _ := store.GetReviewRules(ctx)
	if len(rules.Species) != 1 || rules.Species[0].ScientificName != "Bubo virginianus" || rules.Species[0].Source != db.RuleSourceReview {
		t.Errorf("rules species = %+v, want the owl added by review", rules.Species)
	}

	// A reviewer overrules one detection on its own page; undoing that puts
	// it back to its bout's verdict, not to unreviewed.
	path := "/api/v1/detections/" + b.UploadID + "/det_owl1/review"
	for _, step := range []struct{ send, want string }{{db.ReviewRejected, db.ReviewRejected}, {db.ReviewUnreviewed, db.ReviewConfirmed}} {
		rec := do(t, mux, http.MethodPut, path, `{"status":"`+step.send+`"}`, jane)
		if d := decodeInto[struct {
			Detection Detection `json:"detection"`
		}](t, rec).Detection; rec.Code != http.StatusOK || d.ReviewStatus != step.want {
			t.Errorf("PUT %s = %d %s; want %s", step.send, rec.Code, rec.Body, step.want)
		}
	}

	// A card being deleted is gone from review.
	if _, err := store.UpdateUpload(ctx, b.UploadID, func(u *db.Upload) error {
		u.Status = db.StatusDeleting
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, mux, http.MethodGet, bouts, "", jane); rec.Code != http.StatusNotFound {
		t.Errorf("GET bouts of a card being deleted = %d, want 404", rec.Code)
	}
	if got := decodeInto[struct {
		Cards []ReviewCard `json:"cards"`
	}](t, do(t, mux, http.MethodGet, cards, "", jane)); len(got.Cards) != 0 {
		t.Errorf("cards = %+v, want none while it is deleted", got.Cards)
	}
}
