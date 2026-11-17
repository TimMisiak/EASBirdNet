package review

import (
	"strings"
	"testing"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
)

// testRules accept crows over 0.5, review owls over 0.25, and review anything
// else over 0.5.
func testRules() db.ReviewRules {
	return db.ReviewRules{
		Version:        3,
		Defaults:       db.RuleAction{Action: db.RouteReview, MinConfidence: 0.5},
		BoutGapMinutes: DefaultBoutGapMinutes, SpotChecksPerSpecies: DefaultSpotChecks,
		Species: []db.SpeciesRule{
			{ScientificName: "Corvus brachyrhynchos", CommonName: "American Crow", RuleAction: db.RuleAction{Action: db.RouteAccept, MinConfidence: 0.5}},
			{ScientificName: "Bubo virginianus", CommonName: "Great Horned Owl", RuleAction: db.RuleAction{Action: db.RouteReview, MinConfidence: 0.25}},
		},
	}
}

func TestRouting(t *testing.T) {
	rules := testRules()
	table := NewTable(&rules)
	at := func(scientific string, confidence float64) db.Detection {
		return db.Detection{ScientificName: scientific, Confidence: confidence, ReviewStatus: db.ReviewUnreviewed}
	}
	perch := at("Corvus brachyrhynchos", 0.9)
	perch.Model = db.ModelPerch
	// A reviewer's correction doesn't change where BirdNET's word sends it.
	corrected := at("Corvus brachyrhynchos", 0.9)
	corrected.Review = &db.Review{CorrectedScientificName: "Corvus corax"}
	corrected.ReviewStatus = db.ReviewConfirmed

	for _, c := range []struct {
		name          string
		d             db.Detection
		route, status string
	}{
		{"a common bird", at("Corvus brachyrhynchos", 0.9), db.RouteAccept, db.ReviewAccepted},
		{"a common bird at its floor", at("Corvus brachyrhynchos", 0.5), db.RouteAccept, db.ReviewAccepted},
		{"a common bird below its floor", at("Corvus brachyrhynchos", 0.49), db.RouteIgnore, db.ReviewUnreviewed},
		{"an owl", at("Bubo virginianus", 0.3), db.RouteReview, db.ReviewUnreviewed},
		{"an owl below even its floor", at("Bubo virginianus", 0.2), db.RouteIgnore, db.ReviewUnreviewed},
		{"a species with no row", at("Poecile gambeli", 0.6), db.RouteReview, db.ReviewUnreviewed},
		{"a species with no row, faint", at("Poecile gambeli", 0.3), db.RouteIgnore, db.ReviewUnreviewed},
		{"a reviewed one keeps its verdict", corrected, db.RouteAccept, db.ReviewConfirmed},
		{"Perch's", perch, "", db.ReviewUnreviewed},
	} {
		d := c.d
		table.Apply(&d)
		if d.Route != c.route || d.ReviewStatus != c.status {
			t.Errorf("%s: %q, %s; want %q, %s", c.name, d.Route, d.ReviewStatus, c.route, c.status)
		}
	}

	// An accepted detection a rule change sends to review is unreviewed again.
	d := at("Corvus brachyrhynchos", 0.9)
	table.Apply(&d)
	rules.Species[0].Action = db.RouteReview
	if changed := NewTable(&rules).Apply(&d); !changed || d.ReviewStatus != db.ReviewUnreviewed || d.Route != db.RouteReview {
		t.Errorf("after the crow's rule changed: changed %v, %q, %s; want it routed to review, unreviewed", changed, d.Route, d.ReviewStatus)
	}
	if NewTable(&rules).Apply(&d) {
		t.Error("applying the same rules twice changed it twice")
	}

	// No rules route nothing.
	var none *Table
	d = at("Corvus brachyrhynchos", 0.9)
	if none.Apply(&d) || d.Route != "" || d.ReviewStatus != db.ReviewUnreviewed || none.Version() != 0 {
		t.Errorf("with no rules: %q, %s", d.Route, d.ReviewStatus)
	}
}

func TestCheck(t *testing.T) {
	good := testRules()
	good.Species[0].ScientificName = "  Corvus brachyrhynchos "
	good.Species[1].CommonName = ""
	if err := Check(&good); err != nil {
		t.Fatalf("good rules refused: %v", err)
	}
	if good.Species[0].ScientificName != "Corvus brachyrhynchos" || good.Species[1].CommonName != "Bubo virginianus" {
		t.Errorf("names = %q, %q; want trimmed, and the scientific name standing in for a missing common one",
			good.Species[0].ScientificName, good.Species[1].CommonName)
	}

	for _, c := range []struct {
		name   string
		change func(*db.ReviewRules)
		says   string
	}{
		{"an unknown action", func(r *db.ReviewRules) { r.Species[0].Action = "ignore" }, `"accept" or "review"`},
		{"a floor over 1", func(r *db.ReviewRules) { r.Defaults.MinConfidence = 1.5 }, "from 0 to 1"},
		{"a species twice", func(r *db.ReviewRules) { r.Species[1].ScientificName = r.Species[0].ScientificName }, "listed twice"},
		{"a species with no name", func(r *db.ReviewRules) { r.Species[1].ScientificName = " " }, "no scientific name"},
		{"no bout gap", func(r *db.ReviewRules) { r.BoutGapMinutes = 0 }, "bout gap"},
		{"too many spot-checks", func(r *db.ReviewRules) { r.SpotChecksPerSpecies = 50 }, "spot-checks"},
	} {
		r := testRules()
		c.change(&r)
		if err := Check(&r); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want an error saying %q", c.name, err, c.says)
		}
	}
}
