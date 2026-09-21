package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// Page sizes for a list of detections.
const (
	defaultDetectionsLimit = 50
	maxDetectionsLimit     = 500
)

// defaultDetectionsDays bounds the list of every detection when the request
// names no date range of its own. Without it the query is a cross-partition
// read of every detection ever stored, decoded whole into the memory of the
// one replica that is also running BirdNET. The window is what the list is for
// anyway (what has been heard lately, waiting to be reviewed); looking further
// back is a filter you set.
//
// A week, because a recorder yields on the order of 4,000 detections a day
// (TODO.md 3.4), so five of them fill db.MaxDetectionScan in under a fortnight
// and a month of them is past it. This is the window a request that names no
// dates falls back to; what the tab opens on is detection-list.js's
// DEFAULT_DAYS, which matches it.
const defaultDetectionsDays = 7

// msgTooManyDetections answers a filter that matches more detections than the
// replica will read at once (db.ErrTooMany). It names the ways out that the
// tab actually offers, because a reviewer meeting this has widened the dates.
const msgTooManyDetections = "that is more detections than can be listed at once; narrow the dates, or filter by review, species or confidence"

// detectionQuery is what GET /detections was asked for.
type detectionQuery struct {
	filter  db.DetectionFilter
	species string // scientific name, as BirdNET labelled it
	sort    string
	desc    bool
	limit   int
	offset  int
	// windowed is set when since is the server's default window rather than
	// the request's own, so the answer can say what it was bounded to.
	windowed bool
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
// date range is what keeps it cheap -- which is why a request that names none
// is answered for the last defaultDetectionsDays, and says so in window.
func (h *handlers) listDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	q, problem := parseDetectionQuery(r, h.now())
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	ctx := r.Context()
	found, err := h.store.ListDetections(ctx, q.filter)
	switch {
	case errors.Is(err, db.ErrTooMany):
		h.problem(w, http.StatusBadRequest, msgTooManyDetections)
		return
	case err != nil:
		h.fail(w, r, err)
		return
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

	// Where each row was recorded comes from the cards on this page alone: a
	// point read each, and a page is usually one or two cards. Reading every
	// card to name a few was a second cross-partition query on every request.
	name := map[string]string{}
	rows := make([]ListedDetection, len(page))
	for i, d := range page {
		station, known := name[d.UploadID]
		if !known {
			u, err := h.store.GetUpload(ctx, d.UploadID)
			// A detection whose card has gone keeps its recorder id.
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				h.fail(w, r, err)
				return
			}
			station = u.Recorder.Name
			name[d.UploadID] = station
		}
		rows[i] = ListedDetection{
			Detection: detectionOf(d), Reference: d.UploadID,
			StationName: cmp.Or(station, d.RecorderID), Night: d.Night,
		}
	}

	body := map[string]any{"detections": rows, "total": len(matched), "species": species}
	if q.windowed {
		body["window"] = DetectionWindow{Since: q.filter.Since, Days: defaultDetectionsDays}
	}
	h.json(w, http.StatusOK, body)
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
//
// A request with no since is answered for the defaultDetectionsDays before
// until, or before now; q.windowed says that happened. now is the clock.
func parseDetectionQuery(r *http.Request, now time.Time) (detectionQuery, string) {
	v := r.URL.Query()
	q := detectionQuery{species: v.Get("species"), sort: cmp.Or(v.Get("sort"), sortHeard)}
	limit, offset, problem := parsePage(v)
	if problem != "" {
		return q, problem
	}
	q.limit, q.offset = limit, offset

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
	// No date range asked for: bound it rather than reading every detection
	// ever stored. See defaultDetectionsDays.
	if q.filter.Since.IsZero() {
		q.filter.Since = cmp.Or(q.filter.Until, now).UTC().Truncate(time.Second).AddDate(0, 0, -defaultDetectionsDays)
		q.windowed = true
	}
	return q, ""
}

// parsePage reads the limit and offset a list of detections was asked for, or
// says what is wrong with them. Both are optional, and both lists of
// detections take them, so both are capped the same way.
func parsePage(v url.Values) (limit, offset int, problem string) {
	limit = defaultDetectionsLimit
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxDetectionsLimit {
			return 0, 0, fmt.Sprintf("limit must be a whole number from 1 to %d", maxDetectionsLimit)
		}
		limit = n
	}
	if s := v.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, 0, "offset must be a whole number from 0"
		}
		offset = n
	}
	return limit, offset, ""
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
