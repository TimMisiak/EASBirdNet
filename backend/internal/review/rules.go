package review

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// The rules' defaults, for a first table and for fields a save leaves out.
const (
	// DefaultMinConfidence is the floor for a species with no row: below it
	// a detection of something unexpected isn't worth a reviewer's time.
	// Measured on two weeks at Marymoor, it cuts what goes to review from
	// ~2,700 detections to ~830 (REVIEW.md).
	DefaultMinConfidence = 0.5
	// DefaultBoutGapMinutes and DefaultSpotChecks are where bouts start until
	// a real card says otherwise (REVIEW.md, Measure before tuning).
	DefaultBoutGapMinutes = 15
	DefaultSpotChecks     = 3
)

// Limits on what a save may set.
const (
	maxBoutGapMinutes = 240
	maxSpotChecks     = 20
	maxSpeciesRules   = 2000
)

// Table is the review rules, ready to route detections with. A nil Table is
// no rules: it routes nothing and leaves every detection as it is.
type Table struct {
	Rules   db.ReviewRules
	species map[string]db.SpeciesRule
}

// NewTable is rules as a Table; nil for nil, which is no rules.
func NewTable(rules *db.ReviewRules) *Table {
	if rules == nil {
		return nil
	}
	t := &Table{Rules: *rules, species: make(map[string]db.SpeciesRule, len(rules.Species))}
	for _, r := range rules.Species {
		t.species[r.ScientificName] = r
	}
	return t
}

// Version is the version of the rules routing by t, 0 for none.
func (t *Table) Version() int {
	if t == nil {
		return 0
	}
	return t.Rules.Version
}

// Rule is what the rules say for a species: its own row, or the defaults.
func (t *Table) Rule(scientificName string) db.RuleAction {
	if r, ok := t.species[scientificName]; ok {
		return r.RuleAction
	}
	return t.Rules.Defaults
}

// Route is where the rules send a detection: RouteIgnore below its species'
// floor, otherwise its species' action. It goes by the species BirdNET
// labelled it with, not a reviewer's correction: routing is about how far
// BirdNET's word can be taken. Perch's detections aren't routed (""), nor is
// anything with no rules.
func (t *Table) Route(d db.Detection) string {
	if t == nil || d.ModelOf() != db.ModelBirdNET {
		return ""
	}
	rule := t.Rule(d.ScientificName)
	if d.Confidence < rule.MinConfidence {
		return db.RouteIgnore
	}
	return rule.Action
}

// StatusOf is the review status a route gives a detection no person has
// reviewed: accepted for one the rules accept, unreviewed for anything else.
func StatusOf(route string) string {
	if route == db.RouteAccept {
		return db.ReviewAccepted
	}
	return db.ReviewUnreviewed
}

// Apply routes d, and gives it the status its route does unless a person has
// reviewed it, whose word stands. It says whether d changed. With no rules it
// changes nothing.
func (t *Table) Apply(d *db.Detection) bool {
	if t == nil || d.ModelOf() != db.ModelBirdNET {
		return false
	}
	route, status := t.Route(*d), d.ReviewStatus
	if d.Review == nil {
		status = StatusOf(route)
	}
	changed := route != d.Route || status != d.ReviewStatus
	d.Route, d.ReviewStatus = route, status
	return changed
}

// Check says what is wrong with rules a person is saving, or nil. Names are
// trimmed, and a missing common name is the scientific one.
func Check(r *db.ReviewRules) error {
	var problems []string
	checkAction := func(what string, a db.RuleAction) {
		if a.Action != db.RouteAccept && a.Action != db.RouteReview {
			problems = append(problems, fmt.Sprintf("%s: the action must be %q or %q", what, db.RouteAccept, db.RouteReview))
		}
		if a.MinConfidence < 0 || a.MinConfidence > 1 {
			problems = append(problems, what+": the minimum confidence must be from 0 to 1")
		}
	}
	checkAction("the default", r.Defaults)
	if r.BoutGapMinutes < 1 || r.BoutGapMinutes > maxBoutGapMinutes {
		problems = append(problems, fmt.Sprintf("the bout gap must be from 1 to %d minutes", maxBoutGapMinutes))
	}
	if r.SpotChecksPerSpecies < 0 || r.SpotChecksPerSpecies > maxSpotChecks {
		problems = append(problems, fmt.Sprintf("spot-checks per species must be from 0 to %d", maxSpotChecks))
	}
	if len(r.Species) > maxSpeciesRules {
		problems = append(problems, fmt.Sprintf("at most %d species", maxSpeciesRules))
	}
	seen := map[string]bool{}
	for i := range r.Species {
		s := &r.Species[i]
		s.ScientificName = strings.TrimSpace(s.ScientificName)
		s.CommonName = strings.TrimSpace(s.CommonName)
		s.Note = strings.TrimSpace(s.Note)
		if s.ScientificName == "" {
			problems = append(problems, fmt.Sprintf("species %d has no scientific name", i+1))
			continue
		}
		if s.CommonName == "" {
			s.CommonName = s.ScientificName
		}
		if seen[s.ScientificName] {
			problems = append(problems, s.ScientificName+" is listed twice")
		}
		seen[s.ScientificName] = true
		checkAction(s.ScientificName, s.RuleAction)
	}
	if len(problems) == 0 {
		return nil
	}
	if len(problems) > 5 {
		problems = append(problems[:5], fmt.Sprintf("and %d more", len(problems)-5))
	}
	return errors.New(strings.Join(problems, "; "))
}
