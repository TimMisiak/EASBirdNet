package review

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

var boutCard = db.Upload{ID: card, RecorderID: "SW-01"}

// member is a detection of species heard for seconds from at.
func member(id, species string, at time.Time, seconds int, confidence float64) Member {
	return Member{ID: id, ScientificName: species, CommonName: species, Start: at,
		End: at.Add(time.Duration(seconds) * time.Second), Confidence: confidence}
}

func boutIDs(bs []db.Bout) [][]string {
	var out [][]string
	for _, b := range bs {
		out = append(out, b.DetectionIDs)
	}
	return out
}

func TestBouts(t *testing.T) {
	minute := func(m int) time.Time { return nextMorning.Add(time.Duration(m) * time.Minute) }
	members := []Member{
		// Out of order, as a card's files are read.
		member("o3", "Bubo virginianus", minute(20), 3, 0.4),
		member("o1", "Bubo virginianus", minute(0), 3, 0.3),
		member("o2", "Bubo virginianus", minute(10), 3, 0.9),
		// More than 15 minutes after o3 ends: a bout of its own.
		member("o4", "Bubo virginianus", minute(36), 3, 0.5),
		// A run BirdNET heard for four minutes: the gap is from its end.
		member("s1", "Strix varia", minute(0), 240, 0.6),
		member("s2", "Strix varia", minute(18), 3, 0.7),
	}
	bouts := Bouts(boutCard, db.BoutReview, members, 15*time.Minute)
	want := [][]string{{"o1", "o2", "o3"}, {"s1", "s2"}, {"o4"}}
	if got := boutIDs(bouts); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("bouts = %v, want %v", got, want)
	}
	owls := bouts[0]
	if owls.ID != db.BoutID(card, "Bubo virginianus", "o1") || owls.Kind != db.BoutReview || owls.RecorderID != "SW-01" ||
		owls.Status != db.BoutOpen || !owls.FirstHeardAt.Equal(minute(0)) || !owls.LastHeardAt.Equal(minute(20)) {
		t.Errorf("first bout = %+v", owls)
	}
	if !slices.Equal(owls.Best, []string{"o2", "o3", "o1"}) || owls.MaxConfidence != 0.9 {
		t.Errorf("best = %v at %v, want most confident first", owls.Best, owls.MaxConfidence)
	}

	// A species heard without a break for longer than a bout may hold.
	var chorus []Member
	for i := range maxBoutDetections + 5 {
		chorus = append(chorus, member(fmt.Sprintf("c%05d", i), "Corvus brachyrhynchos", nextMorning.Add(time.Duration(i)*time.Minute), 3, 0.8))
	}
	if got := Bouts(boutCard, db.BoutSpotCheck, chorus, 15*time.Minute); len(got) != 2 ||
		len(got[0].DetectionIDs) != maxBoutDetections || len(got[1].DetectionIDs) != 5 {
		t.Errorf("a chorus too long for one bout made %d bouts", len(got))
	}
}

func TestSpotChecks(t *testing.T) {
	var members []Member
	for i := range 6 {
		at := nextMorning.Add(time.Duration(i) * time.Hour)
		members = append(members,
			member(fmt.Sprintf("crow%d", i), "Corvus brachyrhynchos", at, 3, 0.8),
			member(fmt.Sprintf("goose%d", i), "Branta canadensis", at, 3, 0.8))
	}
	members = append(members, member("finch", "Haemorhous mexicanus", nextMorning, 3, 0.8))
	bouts := Bouts(boutCard, db.BoutSpotCheck, members, 15*time.Minute)

	picked := SpotChecks(bouts, 3, map[string]int{"Branta canadensis": 2})
	per := map[string]int{}
	for _, b := range picked {
		per[b.ScientificName]++
	}
	if per["Corvus brachyrhynchos"] != 3 || per["Branta canadensis"] != 1 || per["Haemorhous mexicanus"] != 1 {
		t.Errorf("picked %v; want 3 crows, 1 goose (2 already heard), the 1 finch there is", per)
	}
	// The same bouts, in another order, pick the same ones.
	slices.Reverse(bouts)
	again := SpotChecks(bouts, 3, map[string]int{"Branta canadensis": 2})
	if !slices.EqualFunc(boutIDs(again), boutIDs(picked), slices.Equal) {
		t.Errorf("picked %v, then %v", boutIDs(picked), boutIDs(again))
	}
	if got := SpotChecks(bouts, 0, nil); len(got) != 0 {
		t.Errorf("no spot-checks asked for, %d picked", len(got))
	}
}
