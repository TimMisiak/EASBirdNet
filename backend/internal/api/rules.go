package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
	"github.com/EASBirdSense/BirdSense/backend/internal/review"
)

// The review rules (REVIEW.md): which species are accepted on BirdNET's word
// and which a reviewer hears, each above a confidence floor. A reviewer may
// read them; an admin or birding expert sets them. Saving them wakes the
// preparer, which routes every card in review by them again.

// ReviewRules is the rules as the API answers them.
type ReviewRules struct {
	Version              int           `json:"version"`
	Defaults             db.RuleAction `json:"defaults"`
	BoutGapMinutes       int           `json:"boutGapMinutes"`
	SpotChecksPerSpecies int           `json:"spotChecksPerSpecies"`
	Species              []SpeciesRule `json:"species"`
	UpdatedBy            string        `json:"updatedBy,omitempty"`
	UpdatedAt            *time.Time    `json:"updatedAt,omitempty"`
}

// SpeciesRule is one species' rule.
type SpeciesRule struct {
	ScientificName string  `json:"scientificName"`
	CommonName     string  `json:"commonName"`
	Action         string  `json:"action"`
	MinConfidence  float64 `json:"minConfidence"`
	Note           string  `json:"note,omitempty"`
	Source         string  `json:"source,omitempty"`
}

// SuggestedRules are the defaults a first table starts from.
type SuggestedRules struct {
	Defaults             db.RuleAction `json:"defaults"`
	BoutGapMinutes       int           `json:"boutGapMinutes"`
	SpotChecksPerSpecies int           `json:"spotChecksPerSpecies"`
}

func rulesOf(r db.ReviewRules) ReviewRules {
	out := ReviewRules{
		Version: r.Version, Defaults: r.Defaults, BoutGapMinutes: r.BoutGapMinutes,
		SpotChecksPerSpecies: r.SpotChecksPerSpecies, Species: make([]SpeciesRule, len(r.Species)),
		UpdatedBy: r.UpdatedByName, UpdatedAt: &r.UpdatedAt,
	}
	for i, s := range r.Species {
		out.Species[i] = SpeciesRule{
			ScientificName: s.ScientificName, CommonName: s.CommonName,
			Action: s.Action, MinConfidence: s.MinConfidence, Note: s.Note, Source: s.Source,
		}
	}
	return out
}

// getReviewRules answers {rules}, which is null until they are first saved,
// with suggested defaults to start a table from.
func (h *handlers) getReviewRules(w http.ResponseWriter, r *http.Request, _ db.User) {
	rules, err := h.store.GetReviewRules(r.Context())
	if errors.Is(err, db.ErrNotFound) {
		h.json(w, http.StatusOK, map[string]any{"rules": nil, "suggested": suggestedRules()})
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.json(w, http.StatusOK, map[string]any{"rules": rulesOf(rules), "suggested": suggestedRules()})
}

func suggestedRules() SuggestedRules {
	return SuggestedRules{
		Defaults:       db.RuleAction{Action: db.RouteReview, MinConfidence: review.DefaultMinConfidence},
		BoutGapMinutes: review.DefaultBoutGapMinutes, SpotChecksPerSpecies: review.DefaultSpotChecks,
	}
}

// saveReviewRules replaces the rules with the body, which carries the version
// it was read at (0 for the first save). A version that isn't the one stored
// any more is a 409: someone else saved them meanwhile.
func (h *handlers) saveReviewRules(w http.ResponseWriter, r *http.Request, me db.User) {
	var body ReviewRules
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	rules := db.ReviewRules{
		Version: body.Version, Defaults: body.Defaults, BoutGapMinutes: body.BoutGapMinutes,
		SpotChecksPerSpecies: body.SpotChecksPerSpecies, Species: make([]db.SpeciesRule, len(body.Species)),
		UpdatedBy: me.ID, UpdatedByName: me.Name,
	}
	for i, s := range body.Species {
		rules.Species[i] = db.SpeciesRule{
			ScientificName: s.ScientificName, CommonName: s.CommonName,
			RuleAction: db.RuleAction{Action: s.Action, MinConfidence: s.MinConfidence}, Note: s.Note, Source: s.Source,
		}
	}
	if err := review.Check(&rules); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	// Kept in name order, so two saves of the same table are the same
	// document, and a diff of two versions reads.
	slices.SortFunc(rules.Species, func(a, b db.SpeciesRule) int {
		return cmp.Compare(a.ScientificName, b.ScientificName)
	})
	if err := h.keepAddedRows(r, &rules); err != nil {
		h.fail(w, r, err)
		return
	}
	saved, err := h.store.SaveReviewRules(r.Context(), rules)
	switch {
	case errors.Is(err, db.ErrConflict):
		h.problem(w, http.StatusConflict, "someone else saved the rules since you opened them; reload to see theirs")
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	h.log.Info("review rules saved", "version", saved.Version, "by", me.ID, "species", len(saved.Species))
	if h.preparer != nil {
		h.preparer.Wake()
	}
	h.json(w, http.StatusOK, map[string]any{"rules": rulesOf(saved)})
}

// keepAddedRows lets a save from the page go over versions that only
// reviewers' verdicts made since it was read (review.AddConfirmed), keeping
// the rows they added; a version a person saved since is still a conflict.
func (h *handlers) keepAddedRows(r *http.Request, rules *db.ReviewRules) error {
	stored, err := h.store.GetReviewRules(r.Context())
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rules.Version == stored.Version || rules.Version == 0 || rules.Version < stored.Saved() {
		return nil
	}
	for _, s := range stored.Species {
		if s.AddedIn > rules.Version && !slices.ContainsFunc(rules.Species, func(t db.SpeciesRule) bool { return t.ScientificName == s.ScientificName }) {
			s.AddedIn = 0
			rules.Species = append(rules.Species, s)
		}
	}
	slices.SortFunc(rules.Species, func(a, b db.SpeciesRule) int {
		return cmp.Compare(a.ScientificName, b.ScientificName)
	})
	rules.Version = stored.Version
	return nil
}

// SpeciesHeard is one species BirdNET labelled detections with lately, as the
// rules page shows it beside that species' rule.
type SpeciesHeard struct {
	ScientificName string `json:"scientificName"`
	CommonName     string `json:"commonName"`
	Detections     int    `json:"detections"`
	// Days is how many Pacific dates it was heard on.
	Days      int `json:"days"`
	Confirmed int `json:"confirmed"`
	Rejected  int `json:"rejected"`
	// ByConfidence is its detections in twenty bands of BirdNET's
	// confidence, [0, 0.05) to [0.95, 1], so the page can say how many a
	// floor keeps without asking again.
	ByConfidence [confidenceBands]int `json:"byConfidence"`
}

const (
	confidenceBands     = 20
	defaultSpeciesDays  = 28
	maxSpeciesHeardDays = 31
)

// speciesHeard answers what BirdNET heard in the last `days` days (28 by
// default, 31 at most), species by species: {since, until, species}. It
// streams the range a page at a time, as the MCP summary does, and shares its
// limit on reads at once.
func (h *handlers) speciesHeard(w http.ResponseWriter, r *http.Request, _ db.User) {
	days := defaultSpeciesDays
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxSpeciesHeardDays {
			h.problem(w, http.StatusBadRequest, "days must be from 1 to 31")
			return
		}
		days = n
	}
	select {
	case h.scans <- struct{}{}:
		defer func() { <-h.scans }()
	case <-r.Context().Done():
		return
	}
	until := h.now().UTC()
	since := until.AddDate(0, 0, -days)
	type tally struct {
		SpeciesHeard
		dates map[string]bool
	}
	bySpecies := map[string]*tally{}
	err := h.store.EachDetection(r.Context(), db.DetectionFilter{Since: since, Until: until, Model: db.ModelBirdNET}, func(d db.Detection) error {
		t := bySpecies[d.ScientificName]
		if t == nil {
			t = &tally{SpeciesHeard: SpeciesHeard{ScientificName: d.ScientificName, CommonName: d.CommonName}, dates: map[string]bool{}}
			bySpecies[d.ScientificName] = t
		}
		t.Detections++
		t.dates[dateOf(d.DetectedAt)] = true
		switch d.ReviewStatus {
		case db.ReviewConfirmed:
			t.Confirmed++
		case db.ReviewRejected:
			t.Rejected++
		}
		t.ByConfidence[min(max(int(d.Confidence*confidenceBands), 0), confidenceBands-1)]++
		return nil
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]SpeciesHeard, 0, len(bySpecies))
	for _, t := range bySpecies {
		t.Days = len(t.dates)
		out = append(out, t.SpeciesHeard)
	}
	slices.SortFunc(out, func(a, b SpeciesHeard) int {
		return cmp.Or(cmp.Compare(b.Detections, a.Detections), cmp.Compare(a.ScientificName, b.ScientificName))
	})
	h.json(w, http.StatusOK, map[string]any{"since": since, "until": until, "species": out})
}
