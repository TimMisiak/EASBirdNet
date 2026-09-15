package api

import (
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
	list := func(query string) listedBody {
		t.Helper()
		rec := do(t, mux, http.MethodGet, "/api/v1/admin/detections"+query, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET detections%s = %d %s", query, rec.Code, rec.Body)
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
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/detections?offset=100", "", admin); !strings.Contains(rec.Body.String(), `"detections":[]`) {
		t.Errorf("past the end = %s, want an empty list", rec.Body)
	}

	for _, query := range []string{
		"?since=yesterday", "?until=2026-09-13", "?status=maybe", "?minConfidence=1.5", "?minConfidence=high",
		"?sort=loudness", "?order=up", "?limit=0", "?limit=501", "?offset=-1",
	} {
		if rec := do(t, mux, http.MethodGet, "/api/v1/admin/detections"+query, "", admin); rec.Code != http.StatusBadRequest {
			t.Errorf("GET detections%s = %d, want 400 (%s)", query, rec.Code, rec.Body)
		}
	}
	vol := signedIn(t, mux, db.RoleVolunteer)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/detections", "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer GET detections = %d, want 403", rec.Code)
	}
}
