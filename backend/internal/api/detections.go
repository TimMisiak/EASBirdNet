package api

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
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
// is answered for the last defaultDetectionsDays, and says so in window. A
// request for one card's is the exception: that is a read of the card's own
// partition, bounded by the card, so it is answered for all of the card.
func (h *handlers) listDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	q, problem := parseDetectionQuery(r, h.now())
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	ctx := r.Context()
	matched, species, err := h.matchDetections(ctx, q)
	switch {
	case errors.Is(err, db.ErrTooMany):
		h.problem(w, http.StatusBadRequest, msgTooManyDetections)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	page := matched[min(q.offset, len(matched)):min(q.offset+q.limit, len(matched))]
	stations, err := h.stationsOf(ctx, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rows := make([]ListedDetection, len(page))
	for i, d := range page {
		rows[i] = listedOf(d, stations)
	}

	body := map[string]any{"detections": rows, "total": len(matched), "species": species}
	if q.windowed {
		body["window"] = DetectionWindow{Since: q.filter.Since, Days: defaultDetectionsDays}
	}
	h.json(w, http.StatusOK, body)
}

// matchDetections is every detection a query matches, sorted, and the species
// heard among them before the species filter. GET /detections pages it and
// GET /detections/export writes it whole, so the two can't disagree about
// what the list holds.
func (h *handlers) matchDetections(ctx context.Context, q detectionQuery) ([]db.Detection, []SpeciesCount, error) {
	found, err := h.store.ListDetections(ctx, q.filter)
	if err != nil {
		return nil, nil, err
	}

	// What was heard on a card being deleted is left out, counts and all.
	// There is rarely such a card, so this is usually an empty query.
	deleting, err := h.store.ListUploads(ctx, db.UploadFilter{Status: db.StatusDeleting})
	if err != nil {
		return nil, nil, err
	}
	going := make(map[string]bool, len(deleting))
	for _, u := range deleting {
		going[u.ID] = true
	}

	// The species come from before the species filter, so picking one still
	// offers the others.
	tally := map[string]*SpeciesCount{}
	matched := found[:0]
	for _, d := range found {
		if going[d.UploadID] {
			continue
		}
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
	return matched, species, nil
}

// stationsOf is where the cards these detections are on were recorded, by
// card, for listedOf to name each row's station.
//
// It reads the cards these rows are on alone: a point read each, and a page is
// usually one or two cards. Reading every card to name a few was a second
// cross-partition query on every request.
func (h *handlers) stationsOf(ctx context.Context, ds []db.Detection) (map[string]string, error) {
	name := map[string]string{}
	for _, d := range ds {
		if _, known := name[d.UploadID]; known {
			continue
		}
		u, err := h.store.GetUpload(ctx, d.UploadID)
		// A detection whose card has gone keeps its recorder id (listedOf).
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		name[d.UploadID] = u.Recorder.Name
	}
	return name, nil
}

// listedOf is a detection as the list shows it, with its card and where that
// card was recorded.
func listedOf(d db.Detection, stations map[string]string) ListedDetection {
	return ListedDetection{
		Detection: detectionOf(d), Reference: d.UploadID,
		StationName: cmp.Or(stations[d.UploadID], d.RecorderID),
	}
}

// exportDetections is GET /detections' whole list as a file to save: every
// detection the same parameters match, in the same order, rather than a page
// of them. limit and offset are ignored.
//
// It costs what one page of the list costs -- the list already reads every
// match to sort and count it -- plus a point read of each card in it, and is
// bounded the same way, by the window and db.MaxDetectionScan. The rows are
// encoded one at a time as they are written, so the answer is never held
// whole a second time beside the detections it is made from.
//
// The file is one object, as every answer is: what was asked for, how many
// matched, and then the detections, as the list's rows.
func (h *handlers) exportDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	q, problem := parseDetectionQuery(r, h.now())
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	ctx := r.Context()
	matched, _, err := h.matchDetections(ctx, q)
	switch {
	case errors.Is(err, db.ErrTooMany):
		h.problem(w, http.StatusBadRequest, msgTooManyDetections)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	// Every read happens before the first byte, so a failure is still an
	// error the browser can show rather than a file cut short.
	stations, err := h.stationsOf(ctx, matched)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	now := h.now().UTC()
	head := DetectionExport{
		ExportedAt: now, Model: q.filter.Model,
		Card: q.filter.UploadID, Status: q.filter.ReviewStatus, MinConfidence: q.filter.MinConfidence, Species: q.species,
		Sort: q.sort, Order: "asc", Total: len(matched),
	}
	if q.desc {
		head.Order = "desc"
	}
	if !q.filter.Since.IsZero() {
		head.Since = &q.filter.Since
	}
	if !q.filter.Until.IsZero() {
		head.Until = &q.filter.Until
	}
	if q.windowed {
		head.Window = &DetectionWindow{Since: q.filter.Since, Days: defaultDetectionsDays}
	}
	opening, err := json.Marshal(head)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="detections-%s.json"`, now.Format("2006-01-02")))
	out := bufio.NewWriterSize(w, 64<<10)
	// The head's own fields, then the detections as its last.
	out.Write(opening[:len(opening)-1])
	out.WriteString(`,"detections":[`)
	for i, d := range matched {
		row, err := json.Marshal(listedOf(d, stations))
		if err != nil {
			h.log.Error("encode exported detection", "id", d.ID, "err", err)
			return
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString("\n")
		if _, err := out.Write(row); err != nil {
			// The browser went away; there is no one to tell.
			return
		}
	}
	out.WriteString("\n]}\n")
	if err := out.Flush(); err != nil {
		h.log.Warn("export detections", "err", err)
	}
}

// parseDetectionQuery reads GET /detections' parameters, or says what is
// wrong with them. Every one is optional:
//
//	since, until   RFC 3339 instants; heard at or after since, and before until
//	status         unreviewed, confirmed or rejected
//	minConfidence  0 to 1
//	species        a scientific name
//	card           a card's reference: only what was heard on it
//	sort           heard (the default), species or confidence
//	order          asc or desc; heard and confidence default to desc, species to asc
//	limit, offset  the page; limit defaults to 50 and is at most 500
//	model          birdnet (the default) or perch: which model's detections
//
// A request with no since and no card is answered for the
// defaultDetectionsDays before until, or before now; q.windowed says that
// happened. now is the clock.
func parseDetectionQuery(r *http.Request, now time.Time) (detectionQuery, string) {
	v := r.URL.Query()
	q := detectionQuery{species: v.Get("species"), sort: cmp.Or(v.Get("sort"), sortHeard)}
	q.filter.UploadID = v.Get("card")
	limit, offset, problem := parsePage(v)
	if problem != "" {
		return q, problem
	}
	q.limit, q.offset = limit, offset
	if q.filter.Model, problem = parseModel(v); problem != "" {
		return q, problem
	}

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
	// ever stored. See defaultDetectionsDays. One card's are bounded already,
	// by the card: a read of its own partition, which is what a card's page
	// links to.
	if q.filter.Since.IsZero() && q.filter.UploadID == "" {
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

// parseModel reads which model's detections a list was asked for. It is one
// model at a time, BirdNET's unless the request says otherwise: Perch is a
// second opinion on the same audio, so the two lists overlap, and a list of
// both would show most birds twice.
func parseModel(v url.Values) (model, problem string) {
	switch m := v.Get("model"); m {
	case "", db.ModelBirdNET:
		return db.ModelBirdNET, ""
	case db.ModelPerch:
		return m, ""
	default:
		return "", fmt.Sprintf("model must be %s or %s", db.ModelBirdNET, db.ModelPerch)
	}
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
