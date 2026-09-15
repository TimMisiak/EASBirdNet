package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// The orders the list of every detection can be sorted in.
const (
	sortHeard      = "heard"
	sortSpecies    = "species"
	sortConfidence = "confidence"
)

// Page sizes for the list of every detection.
const (
	defaultDetectionsLimit = 50
	maxDetectionsLimit     = 500
)

// detectionQuery is what GET /detections was asked for.
type detectionQuery struct {
	filter  db.DetectionFilter
	species string // scientific name, as BirdNET labelled it
	sort    string
	desc    bool
	limit   int
	offset  int
}

// listDetections is every card's detections, filtered, sorted and a page at a
// time, with the species heard among them for the filter to offer.
//
// Species are BirdNET's labels, the names every review page shows, not a
// reviewer's correction: this is the list a reviewer works through, and they
// look for what BirdNET said it heard.
//
// Sorting, the species tally and the page are all cut in Go, from one query:
// Cosmos can't ORDER BY or OFFSET across partitions from the Go SDK (see
// SCHEMA.md). The query carries the date, review and confidence filters, so a
// date range is what keeps it cheap.
func (h *handlers) listDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	q, problem := parseDetectionQuery(r)
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	ctx := r.Context()
	found, err := h.store.ListDetections(ctx, q.filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	uploads, err := h.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stationName := make(map[string]string, len(uploads))
	for _, u := range uploads {
		stationName[u.ID] = u.Recorder.Name
	}

	// The species come from before the species filter, so picking one still
	// offers the others.
	tally := map[string]*SpeciesCount{}
	matched := found[:0]
	for _, d := range found {
		s := tally[d.ScientificName]
		if s == nil {
			s = &SpeciesCount{ScientificName: d.ScientificName, CommonName: d.CommonName}
			tally[d.ScientificName] = s
		}
		s.Detections++
		if q.species == "" || d.ScientificName == q.species {
			matched = append(matched, d)
		}
	}
	species := make([]SpeciesCount, 0, len(tally))
	for _, s := range tally {
		species = append(species, *s)
	}
	slices.SortFunc(species, func(a, b SpeciesCount) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.CommonName), strings.ToLower(b.CommonName)), cmp.Compare(a.ScientificName, b.ScientificName))
	})

	sortListed(matched, q.sort, q.desc)
	page := matched[min(q.offset, len(matched)):min(q.offset+q.limit, len(matched))]
	rows := make([]ListedDetection, len(page))
	for i, d := range page {
		name := stationName[d.UploadID]
		if name == "" {
			name = d.RecorderID
		}
		rows[i] = ListedDetection{Detection: detectionOf(d), Reference: d.UploadID, StationName: name, Night: d.Night}
	}
	h.json(w, http.StatusOK, map[string]any{"detections": rows, "total": len(matched), "species": species})
}

// parseDetectionQuery reads GET /detections' parameters, or says what is
// wrong with them. Every one is optional:
//
//	since, until   RFC 3339 instants; heard at or after since, and before until
//	status         unreviewed, confirmed or rejected
//	minConfidence  0 to 1
//	species        a scientific name
//	sort           heard (the default), species or confidence
//	order          asc or desc; heard and confidence default to desc, species to asc
//	limit, offset  the page; limit defaults to 50 and is at most 500
func parseDetectionQuery(r *http.Request) (detectionQuery, string) {
	v := r.URL.Query()
	q := detectionQuery{species: v.Get("species"), sort: cmp.Or(v.Get("sort"), sortHeard), limit: defaultDetectionsLimit}

	for _, t := range []struct {
		name string
		dst  *time.Time
	}{{"since", &q.filter.Since}, {"until", &q.filter.Until}} {
		if s := v.Get(t.name); s != "" {
			at, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return q, t.name + " must be an RFC 3339 time"
			}
			*t.dst = at
		}
	}
	switch status := v.Get("status"); status {
	case "", db.ReviewUnreviewed, db.ReviewConfirmed, db.ReviewRejected:
		q.filter.ReviewStatus = status
	default:
		return q, "status must be unreviewed, confirmed or rejected"
	}
	if s := v.Get("minConfidence"); s != "" {
		c, err := strconv.ParseFloat(s, 64)
		if err != nil || c < 0 || c > 1 {
			return q, "minConfidence must be a number from 0 to 1"
		}
		q.filter.MinConfidence = c
	}
	switch q.sort {
	case sortHeard, sortConfidence:
		q.desc = true
	case sortSpecies:
	default:
		return q, fmt.Sprintf("sort must be %s, %s or %s", sortHeard, sortSpecies, sortConfidence)
	}
	switch v.Get("order") {
	case "":
	case "asc":
		q.desc = false
	case "desc":
		q.desc = true
	default:
		return q, "order must be asc or desc"
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxDetectionsLimit {
			return q, fmt.Sprintf("limit must be a whole number from 1 to %d", maxDetectionsLimit)
		}
		q.limit = n
	}
	if s := v.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, "offset must be a whole number from 0"
		}
		q.offset = n
	}
	return q, ""
}

// sortListed orders detections by what the list is sorted on. Ties go to the
// most recently heard, then the id, whichever way the list runs, so a page
// holds still between requests.
func sortListed(ds []db.Detection, by string, desc bool) {
	primary := func(a, b db.Detection) int {
		switch by {
		case sortSpecies:
			return cmp.Or(cmp.Compare(strings.ToLower(a.CommonName), strings.ToLower(b.CommonName)), cmp.Compare(a.ScientificName, b.ScientificName))
		case sortConfidence:
			return cmp.Compare(a.Confidence, b.Confidence)
		}
		return a.DetectedAt.Compare(b.DetectedAt)
	}
	slices.SortFunc(ds, func(a, b db.Detection) int {
		c := primary(a, b)
		if desc {
			c = -c
		}
		return cmp.Or(c, b.DetectedAt.Compare(a.DetectedAt), cmp.Compare(a.ID, b.ID))
	})
}
