package main

import (
	"strings"
	"testing"
	"time"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
	"github.com/EASBirdSense/BirdSense/backend/internal/review"
)

// An export as detections_export writes it: a reviewer's correction is in
// scientificName, and bouting goes by BirdNET's.
const export = `id,card,recorderId,recorderName,heardAt,startSec,endSec,scientificName,commonName,birdnetScientificName,birdnetCommonName,confidence,reviewStatus,url
det_1,OWL-1,SW-01,Marymoor,2026-10-01T22:00:00-07:00,0,3,Strix varia,Barred Owl,Bubo virginianus,Great Horned Owl,0.4,confirmed,u
det_2,OWL-1,SW-01,Marymoor,2026-10-01T22:10:00-07:00,600,603,Bubo virginianus,Great Horned Owl,Bubo virginianus,Great Horned Owl,0.5,unreviewed,u
det_3,OWL-1,SW-01,Marymoor,2026-10-01T23:00:00-07:00,3600,3603,Bubo virginianus,Great Horned Owl,Bubo virginianus,Great Horned Owl,0.6,unreviewed,u
det_4,OWL-1,SW-01,Marymoor,2026-10-01T22:00:00-07:00,0,3,Corvus brachyrhynchos,American Crow,Corvus brachyrhynchos,American Crow,0.9,unreviewed,u
det_5,OWL-1,SW-01,Marymoor,2026-10-01T22:00:00-07:00,0,3,Poecile gambeli,Mountain Chickadee,Poecile gambeli,Mountain Chickadee,0.2,unreviewed,u
`

func TestMeasure(t *testing.T) {
	cards := map[string][]db.Detection{}
	if err := readExport(strings.NewReader(export), cards); err != nil {
		t.Fatal(err)
	}
	rules := &db.ReviewRules{
		Defaults: db.RuleAction{Action: db.RouteReview, MinConfidence: 0.5}, BoutGapMinutes: 15, SpotChecksPerSpecies: 3,
		Species: []db.SpeciesRule{
			{ScientificName: "Corvus brachyrhynchos", RuleAction: db.RuleAction{Action: db.RouteAccept, MinConfidence: 0.5}},
			{ScientificName: "Bubo virginianus", RuleAction: db.RuleAction{Action: db.RouteReview, MinConfidence: 0.3}},
		},
	}
	table := review.NewTable(rules)
	card := db.Upload{ID: "OWL-1", RecorderID: "SW-01"}
	for _, c := range []struct {
		gap    time.Duration
		review int
	}{{15 * time.Minute, 2}, {60 * time.Minute, 1}} {
		m := measure(card, table, cards["OWL-1"], c.gap)
		if m.reviewed != 3 || m.accepted != 1 || m.reviewBouts != c.review || m.spotChecks != 1 || m.perSpecies["Great Horned Owl"] != c.review {
			t.Errorf("at %v: %+v; want 3 owls to review in %d bouts, 1 crow checked", c.gap, m, c.review)
		}
	}
	if err := readExport(strings.NewReader("id,card\nx,y\n"), cards); err == nil {
		t.Error("a file that isn't an export was read")
	}
}
