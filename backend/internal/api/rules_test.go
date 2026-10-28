package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

type countingPreparer struct{ wakes atomic.Int64 }

func (p *countingPreparer) Wake() { p.wakes.Add(1) }

type rulesBody struct {
	Rules     *ReviewRules   `json:"rules"`
	Suggested SuggestedRules `json:"suggested"`
}

// A reviewer reads the rules; an admin or birding expert saves them, over the
// version they read; saving wakes the preparer, which routes the cards again.
func TestReviewRules(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.CreateUser(t.Context(), db.User{Name: "Eve Expert", Email: "eve@example.com", Role: db.RoleVolunteer, Roles: []string{db.RoleBirdingExpert}}); err != nil {
		t.Fatal(err)
	}
	preparer := &countingPreparer{}
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: newKeyset(testSessionKey), preparer: preparer, now: func() time.Time { return testNow },
	})
	jane := signInAs(t, mux, "jane@example.com") // a reviewer
	marcus := signInAs(t, mux, "m.lee@example.com")
	eve := signInAs(t, mux, "eve@example.com")

	rec := do(t, mux, http.MethodGet, "/api/v1/review/rules", "", jane)
	got := decodeInto[rulesBody](t, rec)
	if rec.Code != http.StatusOK || got.Rules != nil || got.Suggested.Defaults.MinConfidence != 0.5 || got.Suggested.Defaults.Action != db.RouteReview {
		t.Fatalf("GET rules before any = %d %s; want none, and the suggested defaults", rec.Code, rec.Body)
	}
	if rec := do(t, mux, http.MethodGet, "/api/v1/review/rules", "", marcus); rec.Code != http.StatusForbidden {
		t.Errorf("a field volunteer GET rules = %d, want 403", rec.Code)
	}

	const table = `{"version": %d, "defaults": {"action": "review", "minConfidence": 0.5},
		"boutGapMinutes": 15, "spotChecksPerSpecies": 3, "species": [
		{"scientificName": "Strix varia", "commonName": "Barred Owl", "action": "review", "minConfidence": 0.3},
		{"scientificName": "Corvus brachyrhynchos", "commonName": "American Crow", "action": "accept", "minConfidence": 0.5}]}`
	body := func(version int) string { return fmt.Sprintf(table, version) }

	if rec := do(t, mux, http.MethodPut, "/api/v1/review/rules", body(0), jane); rec.Code != http.StatusForbidden {
		t.Errorf("a reviewer PUT rules = %d, want 403", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/review/rules", strings.Replace(body(0), `"accept"`, `"maybe"`, 1), eve); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT rules with an unknown action = %d, want 400", rec.Code)
	}
	if preparer.wakes.Load() != 0 {
		t.Errorf("refused saves woke the preparer %d times", preparer.wakes.Load())
	}

	rec = do(t, mux, http.MethodPut, "/api/v1/review/rules", body(0), eve)
	saved := decodeInto[rulesBody](t, rec).Rules
	if rec.Code != http.StatusOK || saved == nil || saved.Version != 1 || saved.UpdatedBy != "Eve Expert" ||
		len(saved.Species) != 2 || saved.Species[0].ScientificName != "Corvus brachyrhynchos" {
		t.Fatalf("first save = %d %s; want version 1 by Eve, species in name order", rec.Code, rec.Body)
	}
	if preparer.wakes.Load() != 1 {
		t.Errorf("a save woke the preparer %d times, want once", preparer.wakes.Load())
	}

	// Someone else, saving over the version before, would undo Eve's.
	admin := signedIn(t, mux, db.RoleAdmin)
	if rec := do(t, mux, http.MethodPut, "/api/v1/review/rules", body(0), admin); rec.Code != http.StatusConflict {
		t.Errorf("a save over a version that isn't the latest = %d, want 409", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/review/rules", body(1), admin); rec.Code != http.StatusOK {
		t.Errorf("an admin's save over the latest = %d %s, want 200", rec.Code, rec.Body)
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/review/rules", "", jane)
	if got := decodeInto[rulesBody](t, rec); got.Rules == nil || got.Rules.Version != 2 || got.Rules.UpdatedBy != "Dana Coordinator" {
		t.Errorf("GET rules after two saves = %s", rec.Body)
	}
}

// Undoing a verdict on a detection the rules accept leaves it accepted, as
// the rules had it, not unreviewed.
func TestUndoingAVerdictGoesBackToTheRoute(t *testing.T) {
	mux, store := newTestMux(t)
	ctx := t.Context()
	const ref = "OWL-20260913-SR05"
	crow := db.Detection{
		ID: "det_crow", AudioFileID: "af_1", DetectedAt: testNow.Add(-time.Hour), ScientificName: "Corvus brachyrhynchos",
		CommonName: "American Crow", Confidence: 0.9, Route: db.RouteAccept, ReviewStatus: db.ReviewAccepted,
	}
	if err := store.UpsertDetections(ctx, ref, []db.Detection{crow}); err != nil {
		t.Fatal(err)
	}
	admin := signedIn(t, mux, db.RoleAdmin)
	path := "/api/v1/detections/" + ref + "/det_crow/review"
	for _, step := range []struct{ send, want string }{
		{db.ReviewRejected, db.ReviewRejected},
		{db.ReviewUnreviewed, db.ReviewAccepted},
	} {
		rec := do(t, mux, http.MethodPut, path, `{"status":"`+step.send+`"}`, admin)
		got := decodeInto[struct {
			Detection Detection `json:"detection"`
		}](t, rec).Detection
		if rec.Code != http.StatusOK || got.ReviewStatus != step.want || got.Route != db.RouteAccept {
			t.Errorf("PUT %s = %d %s; want %s, still routed accept", step.send, rec.Code, rec.Body, step.want)
		}
	}
}

// What has been heard lately, species by species, in confidence bands: what
// the rules page shows beside each rule.
func TestSpeciesHeard(t *testing.T) {
	mux, store := newTestMux(t)
	ctx := t.Context()
	const ref = "OWL-20260913-SR05"
	if err := store.UpsertDetections(ctx, ref, []db.Detection{
		{ID: "det_c1", AudioFileID: "af_1", DetectedAt: testNow.Add(-time.Hour), ScientificName: "Corvus brachyrhynchos", CommonName: "American Crow", Confidence: 0.92},
		{ID: "det_c2", AudioFileID: "af_1", DetectedAt: testNow.Add(-26 * time.Hour), ScientificName: "Corvus brachyrhynchos", CommonName: "American Crow", Confidence: 0.31, ReviewStatus: db.ReviewRejected},
		// Too long ago for the default four weeks.
		{ID: "det_c3", AudioFileID: "af_1", DetectedAt: testNow.AddDate(0, 0, -40), ScientificName: "Corvus brachyrhynchos", CommonName: "American Crow", Confidence: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	jane := signedIn(t, mux, db.RoleVolunteer)
	rec := do(t, mux, http.MethodGet, "/api/v1/review/species", "", jane)
	got := decodeInto[struct {
		Species []SpeciesHeard `json:"species"`
	}](t, rec)
	var crow *SpeciesHeard
	for i := range got.Species {
		if got.Species[i].ScientificName == "Corvus brachyrhynchos" {
			crow = &got.Species[i]
		}
	}
	if rec.Code != http.StatusOK || crow == nil || crow.Detections != 2 || crow.Days != 2 || crow.Rejected != 1 ||
		crow.ByConfidence[18] != 1 || crow.ByConfidence[6] != 1 {
		t.Errorf("GET species = %d; crow %+v; want its 2 of the last 4 weeks, on 2 days, one rejected, in bands 18 and 6", rec.Code, crow)
	}
	for _, q := range []string{"?days=0", "?days=32", "?days=week"} {
		if rec := do(t, mux, http.MethodGet, "/api/v1/review/species"+q, "", jane); rec.Code != http.StatusBadRequest {
			t.Errorf("GET species%s = %d, want 400", q, rec.Code)
		}
	}
}
