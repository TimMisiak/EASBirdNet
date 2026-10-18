package api

import (
	"cmp"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// The MCP endpoint is how an LLM tool reads Birdsense, as whoever connected
// it (oauth.go). It has three tools, all read-only:
//
//   - list_recorders: the recorders, and the latest night each has data for.
//   - detection_summary: a range of nights, aggregated here -- by species, by
//     night and recorder, by hour -- small enough to go into a model's
//     context whole. A week of five recorders is ~140,000 detections, which
//     no context holds; its summary is a few thousand tokens.
//   - detections_export: a link to the same range as CSV, one row per
//     detection, for a client that can fetch it and work on it in code.
//
// Both range tools take the same filters (mcpFilter) and read the range
// through once with db.EachDetection, a page at a time: no ceiling, because
// nothing holds the range. What bounds the read is maxMCPNights and mcpScans.

const (
	// defaultMCPNights is the range a request that names none gets: the week
	// of nights before today.
	defaultMCPNights = 7
	// maxMCPNights bounds one request's range. A month of five recorders is
	// ~600,000 detections: a minute or two of reading, which is about what a
	// client will wait for a tool.
	maxMCPNights = 31
	// mcpScans is how many range reads run at once. Each is a cross-partition
	// read of the whole range, so two at once are already most of what
	// serverless Cosmos will serve before throttling; a third waits its turn.
	mcpScans = 2
	// exportLinkLife is how long a detections_export link works. The link is
	// the credential, so it lives long enough to be fetched and no longer.
	exportLinkLife = 30 * time.Minute
	exportPath     = mcpPath + "/export"
)

// mcpInstructions is what a client is told about the server as a whole.
const mcpInstructions = `Birdsense holds BirdNET detections of bird calls from Eastside Audubon's acoustic recorders around East King County, Washington.

Times are Pacific (America/Los_Angeles). Recorders run dusk to dawn, so data is grouped by night: a night is named by the date of the evening it began, and runs from noon that day to noon the next.

Cards are collected from the recorders every week or two, so the most recent nights often have no data yet. detection_summary's coverage says which nights each recorder has analyzed data for, which are still being uploaded or analyzed, and which have none -- read it before drawing conclusions from low counts.

Each detection has a review status: unreviewed (BirdNET's word alone), confirmed (a volunteer listened and agreed), or rejected (a volunteer listened and it wasn't that species). A reviewer's correction replaces BirdNET's species. Confidence is BirdNET's score, 0 to 1; low-confidence detections are often wrong.

Start with list_recorders, then detection_summary for the nights you want. Use detections_export when you need every row and can fetch a URL and process CSV in code.`

// mcpFilter is the range both range tools take.
type mcpFilter struct {
	From          string   `json:"from,omitempty" jsonschema:"the first night to include, YYYY-MM-DD: the date of the evening it began, Pacific time. Defaults to six nights before 'to'."`
	To            string   `json:"to,omitempty" jsonschema:"the last night to include, YYYY-MM-DD, inclusive. Defaults to last night."`
	MinConfidence float64  `json:"minConfidence,omitempty" jsonschema:"only detections BirdNET scored at or above this, from 0 to 1. Defaults to 0: everything stored."`
	Recorders     []string `json:"recorders,omitempty" jsonschema:"recorder ids, from list_recorders, to include. Omit for every recorder."`
}

// mcpRange is an mcpFilter read and checked.
type mcpRange struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	Nights        int      `json:"nights"`
	MinConfidence float64  `json:"minConfidence"`
	Recorders     []string `json:"recorders,omitempty"`
}

// errMCP is a tool call that was asked for something it can't answer. Its
// message goes back to the model, which is meant to correct the call.
type errMCP struct{ msg string }

func (e errMCP) Error() string { return e.msg }

// night is a night's date as a time, at midnight Pacific.
func parseNight(s string) (time.Time, error) {
	return time.ParseInLocation(time.DateOnly, s, pacific)
}

// noon is when a night begins: noon Pacific on the date it is named for. A
// night runs noon to noon, which is how the card scan assigns files to one
// (card-scan.js: a file belongs to the date 12 hours before it began).
func noon(night time.Time) time.Time {
	return time.Date(night.Year(), night.Month(), night.Day(), 12, 0, 0, 0, pacific)
}

// resolve checks a filter against the clock and the recorders there are.
func (f mcpFilter) resolve(now time.Time, recorders []db.Recorder) (mcpRange, error) {
	today := now.In(pacific)
	to := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, pacific).AddDate(0, 0, -1)
	if f.To != "" {
		t, err := parseNight(f.To)
		if err != nil {
			return mcpRange{}, errMCP{"'to' must be a date, YYYY-MM-DD"}
		}
		to = t
	}
	from := to.AddDate(0, 0, -(defaultMCPNights - 1))
	if f.From != "" {
		t, err := parseNight(f.From)
		if err != nil {
			return mcpRange{}, errMCP{"'from' must be a date, YYYY-MM-DD"}
		}
		from = t
	}
	nights := int(math.Round(to.Sub(from).Hours()/24)) + 1
	switch {
	case nights < 1:
		return mcpRange{}, errMCP{"'from' must be on or before 'to'"}
	case nights > maxMCPNights:
		return mcpRange{}, errMCP{fmt.Sprintf("that is %d nights; ask for at most %d at a time", nights, maxMCPNights)}
	case f.MinConfidence < 0 || f.MinConfidence > 1:
		return mcpRange{}, errMCP{"minConfidence must be from 0 to 1"}
	}
	known := map[string]bool{}
	for _, r := range recorders {
		known[r.ID] = true
	}
	var picked []string
	for _, id := range f.Recorders {
		if !known[id] {
			return mcpRange{}, errMCP{fmt.Sprintf("there is no recorder %q; list_recorders gives the ids", id)}
		}
		if !slices.Contains(picked, id) {
			picked = append(picked, id)
		}
	}
	slices.Sort(picked)
	return mcpRange{
		From: from.Format(time.DateOnly), To: to.Format(time.DateOnly), Nights: nights,
		MinConfidence: f.MinConfidence, Recorders: picked,
	}, nil
}

// filter is the store query for a range. The instants are widened by a couple
// of hours either side, and contains trims by night: an hour-long file that
// starts just before noon is the earlier night's to its end.
func (r mcpRange) filter() db.DetectionFilter {
	from, _ := parseNight(r.From)
	to, _ := parseNight(r.To)
	return db.DetectionFilter{
		Since:         noon(from).Add(-2 * time.Hour),
		Until:         noon(to.AddDate(0, 0, 1)).Add(2 * time.Hour),
		MinConfidence: r.MinConfidence,
		// BirdNET's alone: Perch is a second opinion stored beside it, and the
		// two together would count most birds twice (CLAUDE.md, *Perch*).
		Model:       db.ModelBirdNET,
		RecorderIDs: r.Recorders,
	}
}

func (r mcpRange) contains(night string) bool {
	return night >= r.From && night <= r.To
}

// nightList is every night in the range, in order.
func (r mcpRange) nightList() []string {
	from, _ := parseNight(r.From)
	out := make([]string, r.Nights)
	for i := range out {
		out[i] = from.AddDate(0, 0, i).Format(time.DateOnly)
	}
	return out
}

func (r mcpRange) includes(recorderID string) bool {
	return len(r.Recorders) == 0 || slices.Contains(r.Recorders, recorderID)
}

// mcpHandler is the MCP endpoint: the SDK's streamable HTTP transport behind
// the bearer token check.
//
// It is stateless -- every request stands alone, with no session to hold in
// memory -- and answers in plain JSON rather than an event stream, since no
// tool reports progress.
func (h *handlers) mcpHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "birdsense", Title: "Birdsense", Version: "1"},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_recorders",
		Title:       "List recorders",
		Description: "Every recorder, retired ones included: its id (which the other tools filter by), name, position, and the latest night it has data for. Call this first to learn the recorder ids.",
		Annotations: readOnly,
	}, h.toolListRecorders)
	mcp.AddTool(server, &mcp.Tool{
		Name:  "detection_summary",
		Title: "Summarize detections",
		Description: "Aggregates BirdNET's detections over a range of nights (at most 31; the last 7 by default): totals; which nights each recorder has data for; detections per night and recorder; and per species, its count, nights, recorders, confidence, review counts, detections by hour of night, and a link to its most confident detection, where a person can hear the clip. " +
			"Rejected detections are counted in totals.rejected and left out of everything else. To compare with an earlier period, call it again with that range.",
		Annotations: readOnly,
	}, h.toolDetectionSummary)
	mcp.AddTool(server, &mcp.Tool{
		Name:  "detections_export",
		Title: "Export detections as CSV",
		Description: "A link to every BirdNET detection in a range of nights as CSV, one row per detection, rejected ones included -- for analysis in code, when you can fetch a URL. It doesn't put the rows in this conversation: a week can be over 100,000 rows. " +
			"The link works for 30 minutes and needs no other credentials. Columns: " + strings.Join(exportColumns, ", ") + ".",
		Annotations: readOnly,
	}, h.toolDetectionsExport)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The protection is against a web page reaching a server on the
		// machine it runs on through DNS rebinding, which a bearer token
		// already stops; left on, it would refuse requests if an ingress
		// ever connected over loopback.
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
		Logger:                       h.log,
	})
	verify := func(_ context.Context, token string, r *http.Request) (*auth.TokenInfo, error) {
		return h.verifyAccess(r, token)
	}
	return auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: h.oauth.resourceMetadataURL(),
		Scopes:              []string{mcpScope},
	})(handler)
}

// mcpUser is who a tool call is being made as.
func mcpUser(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		return req.Extra.TokenInfo.UserID
	}
	return ""
}

// scan takes one of the mcpScans slots for a range read, waiting for one if
// need be; release gives it back.
func (h *handlers) scan(ctx context.Context) (release func(), err error) {
	select {
	case h.scans <- struct{}{}:
		return func() { <-h.scans }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// --- list_recorders ---

// RecorderInfo is one recorder as list_recorders answers it.
type RecorderInfo struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	AddedOn   string  `json:"addedOn"`
	Retired   bool    `json:"retired,omitempty"`
	// LatestNight is the latest night on any of its cards, and
	// LatestAnalyzedNight the latest on a card BirdNET has finished with.
	LatestNight         string `json:"latestNight,omitempty"`
	LatestAnalyzedNight string `json:"latestAnalyzedNight,omitempty"`
}

type recordersOut struct {
	Recorders []RecorderInfo `json:"recorders"`
}

func (h *handlers) toolListRecorders(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, recordersOut, error) {
	recorders, err := h.store.ListRecorders(ctx)
	if err != nil {
		return nil, recordersOut{}, h.toolFailed("list_recorders", req, err)
	}
	uploads, err := h.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return nil, recordersOut{}, h.toolFailed("list_recorders", req, err)
	}
	latest, analyzed := map[string]string{}, map[string]string{}
	for _, u := range uploads {
		if u.Status == db.StatusDeleting {
			continue
		}
		for _, n := range u.Nights {
			latest[u.RecorderID] = max(latest[u.RecorderID], n.Date)
			if analysisDone(u.Status) {
				analyzed[u.RecorderID] = max(analyzed[u.RecorderID], n.Date)
			}
		}
	}
	out := recordersOut{Recorders: make([]RecorderInfo, len(recorders))}
	for i, r := range recorders {
		out.Recorders[i] = RecorderInfo{
			ID: r.ID, Name: r.Name, Latitude: r.Latitude, Longitude: r.Longitude,
			AddedOn: dateOf(r.CreatedAt), Retired: r.RetiredAt != nil,
			LatestNight: latest[r.ID], LatestAnalyzedNight: analyzed[r.ID],
		}
	}
	return nil, out, nil
}

// analysisDone says whether BirdNET has finished with a card in this status,
// so what it heard on the card is all there.
func analysisDone(status string) bool {
	switch status {
	case db.StatusInReview, db.StatusNeedsAttention, db.StatusResultsSent:
		return true
	}
	return false
}

// toolFailed logs a failure nobody planned for and gives the model a
// sentence instead of the detail.
func (h *handlers) toolFailed(tool string, req *mcp.CallToolRequest, err error) error {
	if errors.Is(err, context.Canceled) {
		return errMCP{"the request was cancelled"}
	}
	h.log.Error("MCP tool failed", "tool", tool, "email", mcpUser(req), "err", err)
	return errMCP{"something went wrong on Birdsense's side; try again"}
}

// --- detection_summary ---

// SummaryOut is detection_summary's answer.
type SummaryOut struct {
	Range    mcpRange           `json:"range"`
	TimeZone string             `json:"timeZone"`
	Totals   SummaryTotals      `json:"totals"`
	Coverage []RecorderCoverage `json:"coverage"`
	// Nights is each recorder's count for each night it heard anything.
	Nights  []NightTally     `json:"nights"`
	Species []SpeciesSummary `json:"species"`
}

// SummaryTotals counts everything in range. Detections leaves out the
// rejected, which Rejected counts.
type SummaryTotals struct {
	Detections int `json:"detections"`
	Species    int `json:"species"`
	Confirmed  int `json:"confirmed"`
	Unreviewed int `json:"unreviewed"`
	Rejected   int `json:"rejected"`
}

// RecorderCoverage is which of the range's nights a recorder has data for:
// on a card BirdNET has finished with (analyzed), on one still being
// uploaded or analyzed (pending), or on no card yet (missing).
type RecorderCoverage struct {
	RecorderID   string   `json:"recorderId"`
	RecorderName string   `json:"recorderName"`
	Analyzed     []string `json:"nightsAnalyzed"`
	Pending      []string `json:"nightsPending"`
	Missing      []string `json:"nightsMissing"`
}

// NightTally is one recorder's night.
type NightTally struct {
	Night      string `json:"night"`
	RecorderID string `json:"recorderId"`
	Detections int    `json:"detections"`
	Species    int    `json:"species"`
}

// SpeciesSummary is one species over the range.
type SpeciesSummary struct {
	ScientificName string `json:"scientificName"`
	CommonName     string `json:"commonName"`
	Detections     int    `json:"detections"`
	Nights         int    `json:"nights"`
	// Recorders is its detections by recorder id.
	Recorders      map[string]int `json:"recorders"`
	Confirmed      int            `json:"confirmed"`
	Unreviewed     int            `json:"unreviewed"`
	MaxConfidence  float64        `json:"maxConfidence"`
	MeanConfidence float64        `json:"meanConfidence"`
	FirstHeard     time.Time      `json:"firstHeard"`
	LastHeard      time.Time      `json:"lastHeard"`
	// ByHour is its detections by the hour of the day they were heard, from
	// 0 (midnight to 1 a.m.) to 23, Pacific.
	ByHour [24]int      `json:"byHour"`
	Best   DetectionRef `json:"best"`
}

// DetectionRef points at one detection's page, where its clip plays.
type DetectionRef struct {
	URL        string    `json:"url"`
	HeardAt    time.Time `json:"heardAt"`
	RecorderID string    `json:"recorderId"`
	Confidence float64   `json:"confidence"`
}

func (h *handlers) toolDetectionSummary(ctx context.Context, req *mcp.CallToolRequest, in mcpFilter) (*mcp.CallToolResult, SummaryOut, error) {
	recorders, err := h.store.ListRecorders(ctx)
	if err != nil {
		return nil, SummaryOut{}, h.toolFailed("detection_summary", req, err)
	}
	rng, err := in.resolve(h.now(), recorders)
	if err != nil {
		return nil, SummaryOut{}, err
	}
	uploads, err := h.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return nil, SummaryOut{}, h.toolFailed("detection_summary", req, err)
	}
	release, err := h.scan(ctx)
	if err != nil {
		return nil, SummaryOut{}, h.toolFailed("detection_summary", req, err)
	}
	defer release()

	started := time.Now()
	s := newSummary(rng, h.oauth.issuer)
	going := deletingCards(uploads)
	err = h.store.EachDetection(ctx, rng.filter(), func(d db.Detection) error {
		if !going[d.UploadID] && rng.contains(d.Night) {
			s.add(d)
		}
		return nil
	})
	if err != nil {
		return nil, SummaryOut{}, h.toolFailed("detection_summary", req, err)
	}
	out := s.result(coverage(rng, recorders, uploads))
	h.log.Info("MCP detection summary", "email", mcpUser(req), "from", rng.From, "to", rng.To,
		"recorders", len(rng.Recorders), "detections", out.Totals.Detections+out.Totals.Rejected,
		"took", time.Since(started).Round(time.Millisecond))
	return nil, out, nil
}

// deletingCards is the cards being deleted, whose detections count for
// nothing (CLAUDE.md, *Deleting a card*).
func deletingCards(uploads []db.Upload) map[string]bool {
	going := map[string]bool{}
	for _, u := range uploads {
		if u.Status == db.StatusDeleting {
			going[u.ID] = true
		}
	}
	return going
}

// summary is detection_summary's tally while the range is read.
type summary struct {
	rng       mcpRange
	publicURL string
	totals    SummaryTotals
	species   map[string]*speciesTally
	// nights counts by night and recorder, with the species heard in each.
	nights map[[2]string]*nightTally
}

type speciesTally struct {
	SpeciesSummary
	nights  map[string]bool
	confSum float64
}

type nightTally struct {
	detections int
	species    map[string]bool
}

func newSummary(rng mcpRange, publicURL string) *summary {
	return &summary{rng: rng, publicURL: publicURL, species: map[string]*speciesTally{}, nights: map[[2]string]*nightTally{}}
}

func (s *summary) add(d db.Detection) {
	switch d.ReviewStatus {
	case db.ReviewRejected:
		s.totals.Rejected++
		return
	case db.ReviewConfirmed:
		s.totals.Confirmed++
	default:
		s.totals.Unreviewed++
	}
	s.totals.Detections++

	// What a reviewer corrected it to, if they did.
	scientific, common := d.Species()
	t := s.species[scientific]
	if t == nil {
		t = &speciesTally{
			SpeciesSummary: SpeciesSummary{ScientificName: scientific, CommonName: common, Recorders: map[string]int{}},
			nights:         map[string]bool{},
		}
		s.species[scientific] = t
	}
	heard := d.DetectedAt.In(pacific)
	t.Detections++
	t.nights[d.Night] = true
	t.Recorders[d.RecorderID]++
	if d.ReviewStatus == db.ReviewConfirmed {
		t.Confirmed++
	} else {
		t.Unreviewed++
	}
	t.confSum += d.Confidence
	t.ByHour[heard.Hour()]++
	if t.FirstHeard.IsZero() || heard.Before(t.FirstHeard) {
		t.FirstHeard = heard
	}
	if heard.After(t.LastHeard) {
		t.LastHeard = heard
	}
	if d.Confidence > t.MaxConfidence || t.Best.URL == "" {
		t.MaxConfidence = d.Confidence
		t.Best = DetectionRef{
			URL: detectionURL(s.publicURL, d), HeardAt: heard,
			RecorderID: d.RecorderID, Confidence: round3(d.Confidence),
		}
	}

	key := [2]string{d.Night, d.RecorderID}
	n := s.nights[key]
	if n == nil {
		n = &nightTally{species: map[string]bool{}}
		s.nights[key] = n
	}
	n.detections++
	n.species[scientific] = true
}

func (s *summary) result(cov []RecorderCoverage) SummaryOut {
	out := SummaryOut{
		Range: s.rng, TimeZone: pacific.String(), Totals: s.totals, Coverage: cov,
		Nights: make([]NightTally, 0, len(s.nights)), Species: make([]SpeciesSummary, 0, len(s.species)),
	}
	out.Totals.Species = len(s.species)
	for key, n := range s.nights {
		out.Nights = append(out.Nights, NightTally{Night: key[0], RecorderID: key[1], Detections: n.detections, Species: len(n.species)})
	}
	slices.SortFunc(out.Nights, func(a, b NightTally) int {
		return cmp.Or(cmp.Compare(a.Night, b.Night), cmp.Compare(a.RecorderID, b.RecorderID))
	})
	for _, t := range s.species {
		t.Nights = len(t.nights)
		t.MaxConfidence = round3(t.MaxConfidence)
		t.MeanConfidence = round3(t.confSum / float64(t.Detections))
		out.Species = append(out.Species, t.SpeciesSummary)
	}
	// The most heard first; the model reads from the top.
	slices.SortFunc(out.Species, func(a, b SpeciesSummary) int {
		return cmp.Or(cmp.Compare(b.Detections, a.Detections), cmp.Compare(a.ScientificName, b.ScientificName))
	})
	return out
}

// coverage is which nights of the range each recorder has data for, from the
// cards' own lists of nights. The recorders are the ones asked for, or every
// recorder still in the field and any retired one with a card in range.
func coverage(rng mcpRange, recorders []db.Recorder, uploads []db.Upload) []RecorderCoverage {
	analyzed, pending := map[string]map[string]bool{}, map[string]map[string]bool{}
	names := map[string]string{}
	for _, r := range recorders {
		names[r.ID] = r.Name
	}
	for _, u := range uploads {
		if u.Status == db.StatusDeleting || !rng.includes(u.RecorderID) {
			continue
		}
		into := pending
		if analysisDone(u.Status) {
			into = analyzed
		}
		for _, n := range u.Nights {
			if !rng.contains(n.Date) {
				continue
			}
			if into[u.RecorderID] == nil {
				into[u.RecorderID] = map[string]bool{}
			}
			into[u.RecorderID][n.Date] = true
		}
	}

	var ids []string
	for _, r := range recorders {
		if rng.includes(r.ID) && (len(rng.Recorders) > 0 || r.RetiredAt == nil || analyzed[r.ID] != nil || pending[r.ID] != nil) {
			ids = append(ids, r.ID)
		}
	}
	out := make([]RecorderCoverage, 0, len(ids))
	for _, id := range ids {
		c := RecorderCoverage{RecorderID: id, RecorderName: names[id], Analyzed: []string{}, Pending: []string{}, Missing: []string{}}
		for _, night := range rng.nightList() {
			switch {
			case analyzed[id][night]:
				c.Analyzed = append(c.Analyzed, night)
			case pending[id][night]:
				c.Pending = append(c.Pending, night)
			default:
				c.Missing = append(c.Missing, night)
			}
		}
		out = append(out, c)
	}
	return out
}

// detectionURL is a detection's page in the app, opened from the detections
// list: its clip, its spectrogram, and Confirm and Discard.
func detectionURL(publicURL string, d db.Detection) string {
	return publicURL + "/app/detections/" + url.PathEscape(d.UploadID) + "/" + url.PathEscape(d.ID)
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

// --- detections_export ---

// ExportOut is detections_export's answer.
type ExportOut struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
	Range     mcpRange  `json:"range"`
	Columns   []string  `json:"columns"`
}

// exportLink is what an export link carries: who asked, and for what.
type exportLink struct {
	Email string   `json:"e"`
	Range mcpRange `json:"r"`
}

var exportColumns = []string{
	"id", "card", "recorderId", "recorderName", "night", "heardAt", "startSec", "endSec",
	"scientificName", "commonName", "birdnetScientificName", "birdnetCommonName",
	"confidence", "reviewStatus", "url",
}

func (h *handlers) toolDetectionsExport(ctx context.Context, req *mcp.CallToolRequest, in mcpFilter) (*mcp.CallToolResult, ExportOut, error) {
	recorders, err := h.store.ListRecorders(ctx)
	if err != nil {
		return nil, ExportOut{}, h.toolFailed("detections_export", req, err)
	}
	rng, err := in.resolve(h.now(), recorders)
	if err != nil {
		return nil, ExportOut{}, err
	}
	expires := h.now().Add(exportLinkLife)
	token := signJSON(h.oauth.exports, exportLink{Email: mcpUser(req), Range: rng}, expires)
	out := ExportOut{
		URL:       h.oauth.issuer + exportPath + "?token=" + url.QueryEscape(token),
		ExpiresAt: expires.UTC().Truncate(time.Second),
		Range:     rng,
		Columns:   exportColumns,
	}
	name := fmt.Sprintf("birdsense-detections-%s-to-%s.csv", rng.From, rng.To)
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: fmt.Sprintf(
			"Every BirdNET detection from the nights of %s to %s is at the link below, as CSV with a header row (columns: %s). "+
				"Fetch it with an HTTP GET -- in code, e.g. curl -o %s '<url>' -- within 30 minutes; it needs no other credentials. "+
				"heardAt is Pacific time with its offset. scientificName and commonName are after review; birdnet* are BirdNET's own label. "+
				"Rows are in no particular order.\n\n%s",
			rng.From, rng.To, strings.Join(exportColumns, ", "), name, out.URL)},
		&mcp.ResourceLink{URI: out.URL, Name: name, Title: "Detections, " + rng.From + " to " + rng.To, MIMEType: "text/csv"},
	}}
	h.log.Info("MCP export link issued", "email", mcpUser(req), "from", rng.From, "to", rng.To, "recorders", len(rng.Recorders))
	return res, out, nil
}

// exportDetections serves an export link: the range as CSV, streamed as it
// is read, so a week costs a page of memory rather than the week. The link
// is the credential (it is signed, says who asked for what, and lapses after
// exportLinkLife), because whatever fetches it -- code in a model's sandbox --
// holds no token. It still answers only while its person is on the roster.
func (h *handlers) exportDetections(w http.ResponseWriter, r *http.Request) {
	var link exportLink
	if err := verifyJSON(h.oauth.exports, r.URL.Query().Get("token"), h.now(), &link); err != nil {
		h.problem(w, http.StatusUnauthorized, "that export link is unknown or has expired; ask for a new one")
		return
	}
	if _, ok, err := h.onRoster(r, link.Email); err != nil {
		h.fail(w, r, err)
		return
	} else if !ok {
		h.problem(w, http.StatusUnauthorized, "that export link's owner is no longer on the roster")
		return
	}
	ctx := r.Context()
	rng := link.Range
	uploads, err := h.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	release, err := h.scan(ctx)
	if err != nil {
		return
	}
	defer release()

	going := deletingCards(uploads)
	station := map[string]string{}
	for _, u := range uploads {
		station[u.ID] = u.Recorder.Name
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="birdsense-detections-%s-to-%s.csv"`, rng.From, rng.To))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	var body io.Writer = w
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		body = gz
	}
	out := csv.NewWriter(body)
	out.Write(exportColumns)

	started, rows := time.Now(), 0
	err = h.store.EachDetection(ctx, rng.filter(), func(d db.Detection) error {
		if going[d.UploadID] || !rng.contains(d.Night) {
			return nil
		}
		scientific, common := d.Species()
		rows++
		out.Write([]string{
			d.ID, d.UploadID, d.RecorderID, cmp.Or(station[d.UploadID], d.RecorderID), d.Night,
			d.DetectedAt.In(pacific).Format(time.RFC3339),
			strconv.FormatFloat(d.StartSec, 'f', -1, 64), strconv.FormatFloat(d.EndSec, 'f', -1, 64),
			scientific, common, d.ScientificName, d.CommonName,
			strconv.FormatFloat(round3(d.Confidence), 'f', -1, 64), d.ReviewStatus,
			detectionURL(h.oauth.issuer, d),
		})
		return out.Error()
	})
	out.Flush()
	if err == nil {
		err = out.Error()
	}
	if err != nil {
		// The header has gone, so the status can't say so. Aborting the
		// response is what makes a client see a failed download rather than a
		// short file that looks whole.
		if !errors.Is(err, context.Canceled) {
			h.log.Error("MCP export failed part way", "email", link.Email, "rows", rows, "err", err)
		}
		panic(http.ErrAbortHandler)
	}
	h.log.Info("MCP export served", "email", link.Email, "from", rng.From, "to", rng.To,
		"rows", rows, "took", time.Since(started).Round(time.Millisecond))
}
