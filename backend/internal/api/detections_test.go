package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

type listedBody struct {
	Detections []ListedDetection `json:"detections"`
	Total      int               `json:"total"`
	Species    []SpeciesCount    `json:"species"`
	Window     *DetectionWindow  `json:"window"`
}

func TestListEveryDetection(t *testing.T) {
	mux, store := newTestMux(t)
	// Two more on top of seedProgram's six, at other confidences.
	mk := func(at string, confidence float64, scientific, common string) db.Detection {
		t.Helper()
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		return db.Detection{
			AudioFileID: "af_list", DetectedAt: when, Night: "2026-08-19", StartSec: float64(when.Hour()),
			ScientificName: scientific, CommonName: common, Confidence: confidence,
		}
	}
	if err := store.UpsertDetections(t.Context(), "OWL-20260821-SR03", []db.Detection{
		mk("2026-08-20T10:00:00Z", 0.3, "Tyto alba", "Barn Owl"),
		mk("2026-08-20T11:00:00Z", 0.97, "Strix varia", "Barred Owl"),
	}); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	// This test is about what the list does with its rows, not about the window
	// a request with no dates falls back to (defaultDetectionsDays, which
	// TestEveryDetectionIsBoundedWithoutDates holds). So every ask here carries
	// a range wide enough for all of seedProgram's, unless it names its own.
	const everything = "since=2026-01-01T00:00:00Z"
	list := func(query string) listedBody {
		t.Helper()
		q := strings.TrimPrefix(query, "?")
		if !strings.Contains(q, "since=") {
			q = everything + "&" + q
		}
		rec := do(t, mux, http.MethodGet, "/api/v1/detections?"+q, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET detections?%s = %d %s", q, rec.Code, rec.Body)
		}
		return decodeInto[listedBody](t, rec)
	}
	commonNames := func(rows []ListedDetection) string {
		names := make([]string, len(rows))
		for i, d := range rows {
			names[i] = d.CommonName
		}
		return strings.Join(names, ", ")
	}

	// By default: every card, newest first, each with its card and where the
	// card says it was recorded.
	all := list("")
	if all.Total != 8 || len(all.Detections) != 8 {
		t.Fatalf("all = %d of %d, want 8", len(all.Detections), all.Total)
	}
	for i := 1; i < len(all.Detections); i++ {
		if all.Detections[i].DetectedAt.After(all.Detections[i-1].DetectedAt) {
			t.Errorf("all detections aren't newest first: %s", commonNames(all.Detections))
			break
		}
	}
	if d := all.Detections[0]; d.CommonName != "Western Screech-Owl" || d.Reference != "OWL-20260913-SR05" ||
		d.StationName != "Soaring Eagle – East Loop" || d.Night != "2026-09-12" {
		t.Errorf("newest = %+v, want the screech-owl on OWL-20260913-SR05, at the card's station name", d)
	}
	// BirdNET's labels, not a reviewer's correction.
	wantSpecies := []SpeciesCount{
		{"Tyto alba", "Barn Owl", 1},
		{"Strix varia", "Barred Owl", 4},
		{"Bubo virginianus", "Great Horned Owl", 1},
		{"Aegolius acadicus", "Northern Saw-whet Owl", 1},
		{"Megascops kennicottii", "Western Screech-Owl", 1},
	}
	if len(all.Species) != len(wantSpecies) {
		t.Fatalf("species = %+v, want %+v", all.Species, wantSpecies)
	}
	for i, s := range wantSpecies {
		if all.Species[i] != s {
			t.Errorf("species[%d] = %+v, want %+v", i, all.Species[i], s)
		}
	}

	// Picking a species still offers the others.
	if got := list("?species=Strix+varia&sort=confidence"); got.Total != 4 || got.Detections[0].Confidence != 0.97 ||
		len(got.Species) != 5 {
		t.Errorf("barred owls by confidence = %d, first at %v, %d species; want 4, 0.97 first, 5 species",
			got.Total, got.Detections[0].Confidence, len(got.Species))
	}
	if got := list("?minConfidence=0.5&sort=confidence&order=asc"); got.Total != 7 ||
		got.Detections[0].Confidence != 0.9 || got.Detections[6].Confidence != 0.97 {
		t.Errorf("from 50%%, least confident first = %d: %s", got.Total, commonNames(got.Detections))
	}
	if got := list("?status=rejected"); got.Total != 1 || got.Detections[0].ReviewStatus != db.ReviewRejected {
		t.Errorf("rejected = %d, want 1", got.Total)
	}
	// since is inclusive and until isn't.
	if got := list("?since=2026-09-12T10:00:00Z&until=2026-09-13T11:00:00Z"); got.Total != 2 ||
		got.Species[0].CommonName != "Barred Owl" || len(got.Species) != 1 {
		t.Errorf("12th 10:00 to 13th 11:00 = %d: %s", got.Total, commonNames(got.Detections))
	}

	// A page of the list by species: barred owls newest first within it.
	page := list("?sort=species&limit=3&offset=3")
	if page.Total != 8 || commonNames(page.Detections) != "Barred Owl, Barred Owl, Great Horned Owl" ||
		page.Detections[1].Confidence != 0.97 {
		t.Errorf("page 2 by species = %d: %s", page.Total, commonNames(page.Detections))
	}
	if rec := do(t, mux, http.MethodGet, "/api/v1/detections?"+everything+"&offset=100", "", admin); !strings.Contains(rec.Body.String(), `"detections":[]`) {
		t.Errorf("past the end = %s, want an empty list", rec.Body)
	}

	for _, query := range []string{
		"?since=yesterday", "?until=2026-09-13", "?status=maybe", "?minConfidence=1.5", "?minConfidence=high",
		"?sort=loudness", "?order=up", "?limit=0", "?limit=501", "?offset=-1",
	} {
		if rec := do(t, mux, http.MethodGet, "/api/v1/detections"+query, "", admin); rec.Code != http.StatusBadRequest {
			t.Errorf("GET detections%s = %d, want 400 (%s)", query, rec.Code, rec.Body)
		}
	}
	// Every card's, for anyone signed in.
	vol := signedIn(t, mux, db.RoleVolunteer)
	if rec := do(t, mux, http.MethodGet, "/api/v1/detections?"+everything, "", vol); rec.Code != http.StatusOK ||
		decodeInto[listedBody](t, rec).Total != 8 {
		t.Errorf("volunteer GET detections = %d %s, want all 8", rec.Code, rec.Body)
	}
	if rec := do(t, mux, http.MethodGet, "/api/v1/detections", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET detections = %d, want 401", rec.Code)
	}
}

// TestEveryDetectionIsBoundedWithoutDates holds the line that keeps the
// Detections tab from reading every detection ever stored on every request:
// with no since, the query is bounded to the last defaultDetectionsDays and
// the answer says so. See defaultDetectionsDays.
func TestEveryDetectionIsBoundedWithoutDates(t *testing.T) {
	mux, store := newTestMux(t)
	// Older than the default window, on a card the seed already has.
	old, err := time.Parse(time.RFC3339, "2026-06-01T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDetections(t.Context(), "OWL-20260821-SR03", []db.Detection{{
		AudioFileID: "af_old", DetectedAt: old, Night: "2026-05-31", StartSec: 9,
		ScientificName: "Tyto alba", CommonName: "Barn Owl", Confidence: 0.8,
	}}); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	list := func(query string) listedBody {
		t.Helper()
		rec := do(t, mux, http.MethodGet, "/api/v1/detections"+query, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET detections%s = %d %s", query, rec.Code, rec.Body)
		}
		return decodeInto[listedBody](t, rec)
	}

	// The default window leaves June out, of the rows and of the species, and
	// August with it: seedProgram's six span a month, and five are this week.
	recent := list("")
	wantSince := testNow.AddDate(0, 0, -defaultDetectionsDays)
	if recent.Total != 5 || recent.Window == nil || !recent.Window.Since.Equal(wantSince) ||
		recent.Window.Days != defaultDetectionsDays {
		t.Errorf("default list = %d detections, window %+v; want 5 since %v", recent.Total, recent.Window, wantSince)
	}
	for _, s := range recent.Species {
		if s.CommonName == "Barn Owl" {
			t.Errorf("the June barn owl is in the default window's species: %+v", recent.Species)
		}
	}

	// Asking for dates is asking for exactly those, and is never windowed.
	all := list("?since=2026-01-01T00:00:00Z")
	if all.Total != 7 || all.Window != nil {
		t.Errorf("since January = %d detections, window %+v; want 7 and no window", all.Total, all.Window)
	}
	// until with no since is the window before until, not everything before it.
	until := time.Date(2026, time.June, 2, 0, 0, 0, 0, time.UTC)
	before := list("?until=" + until.Format(time.RFC3339))
	if before.Total != 1 || before.Window == nil ||
		!before.Window.Since.Equal(until.AddDate(0, 0, -defaultDetectionsDays)) {
		t.Errorf("until June 2nd = %d detections, window %+v; want the one barn owl in the week before it", before.Total, before.Window)
	}
}

// TestCardDetectionsArePaged holds the cap on GET /detections/{ref}: a card
// carries tens of thousands of detections and the whole card is the default
// ask, so the answer is a page with the count beside it.
func TestCardDetectionsArePaged(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	// seedProgram's five on this card, newest first.
	card := func(query string) detectionsBody {
		t.Helper()
		rec := do(t, mux, http.MethodGet, "/api/v1/detections/OWL-20260913-SR05"+query, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET card detections%s = %d %s", query, rec.Code, rec.Body)
		}
		return decodeInto[detectionsBody](t, rec)
	}
	whole := card("")
	if whole.Total != 5 || len(whole.Detections) != 5 {
		t.Fatalf("whole card = %d of %d, want 5", len(whole.Detections), whole.Total)
	}
	page := card("?limit=2&offset=2")
	if page.Total != 5 || len(page.Detections) != 2 || page.Detections[0].ID != whole.Detections[2].ID {
		t.Errorf("page = %d of %d starting %q, want 2 of 5 from %q",
			len(page.Detections), page.Total, page.Detections[0].ID, whole.Detections[2].ID)
	}
	if past := card("?offset=5"); past.Total != 5 || len(past.Detections) != 0 {
		t.Errorf("past the end = %d of %d, want none of 5", len(past.Detections), past.Total)
	}
	for _, query := range []string{"?limit=0", "?limit=501", "?limit=lots", "?offset=-1"} {
		if rec := do(t, mux, http.MethodGet, "/api/v1/detections/OWL-20260913-SR05"+query, "", admin); rec.Code != http.StatusBadRequest {
			t.Errorf("GET card detections%s = %d, want 400 (%s)", query, rec.Code, rec.Body)
		}
	}
}

// tooManyStore is the test program with a detections list too wide to read.
type tooManyStore struct{ db.Store }

func (tooManyStore) ListDetections(context.Context, db.DetectionFilter) ([]db.Detection, error) {
	return nil, fmt.Errorf("%w: more than %d documents match", db.ErrTooMany, db.MaxDetectionScan)
}

// TestTooManyDetectionsIsAskedAgain holds what a reviewer meets when they
// widen the dates past what the replica will read (db.MaxDetectionScan): a 400
// that names the way out, not a 500 that reads as the site being broken -- and
// not the unbounded read that ceiling exists to prevent.
func TestTooManyDetectionsIsAskedAgain(t *testing.T) {
	mux := muxFor(tooManyStore{newTestStore(t)}, testFiles(t), false)
	admin := signedIn(t, mux, db.RoleAdmin)

	rec := do(t, mux, http.MethodGet, "/api/v1/detections?since=2020-01-01T00:00:00Z", "", admin)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "narrow the dates") {
		t.Errorf("a list too wide to read = %d %s, want 400 saying how to narrow it", rec.Code, rec.Body)
	}
	// A card's own list has no dates to narrow, so it is pointed at one file.
	rec = do(t, mux, http.MethodGet, "/api/v1/detections/OWL-20260913-SR05", "", admin)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "?file=") {
		t.Errorf("a card too big to list = %d %s, want 400 pointing at one file", rec.Code, rec.Body)
	}
}
