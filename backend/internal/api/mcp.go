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
//   - list_recorders: the recorders, and when each one last recorded.
//   - detection_summary: a span of time, aggregated here -- by species, by
//     day and recorder, by hour -- small enough to go into a model's context
//     whole. A week of five recorders is ~140,000 detections, which no
//     context holds; its summary is a few thousand tokens.
//   - detections_export: a link to the same span as CSV, one row per
//     detection, for a client that can fetch it and work on it in code.
//
// A span is two instants the caller names, and nothing else: which hours of
// which days make up "last week's nights" is the caller's to work out. The
// time zone the caller names is only how the answer counts days and hours
// and writes its times.
//
// Both range tools take the same filters (mcpFilter) and read the span
// through once with db.EachDetection, a page at a time: no ceiling, because
// nothing holds the span. What bounds the read is maxMCPSpan and mcpScans.

const (
	// defaultMCPSpan is the span a request that names none gets: the week
	// before now.
	defaultMCPSpan = 7 * 24 * time.Hour
	// maxMCPSpan bounds one request's span. A month of five recorders is
	// ~600,000 detections: a minute or two of reading, which is about what a
	// client will wait for a tool.
	maxMCPSpan = 31 * 24 * time.Hour
	// mcpScans is how many range reads run at once. Each is a cross-partition
	// read of the whole span, so two at once are already most of what
	// serverless Cosmos will serve before throttling; a third waits its turn.
	mcpScans = 2
	// exportLinkLife is how long a detections_export link works. The link is
	// the credential, so it lives long enough to be fetched and no longer.
	exportLinkLife = 30 * time.Minute
	exportPath     = mcpPath + "/export"
	// defaultMCPZone is the time zone an answer is given in when the request
	// names none: where the recorders are.
	defaultMCPZone = "America/Los_Angeles"
)

// mcpInstructions is what a client is told about the server as a whole.
const mcpInstructions = `Birdsense holds BirdNET detections of bird calls from Eastside Audubon's acoustic recorders around East King County, Washington (Pacific time, America/Los_Angeles).

Every time range is two RFC 3339 timestamps, since (inclusive) and until (exclusive), and nothing else: choose the instants that cover what you mean. Recorders usually record from dusk to dawn, so an evening and the morning after it fall on two dates. Answers give every time with its UTC offset, and count days and hours in the timeZone you ask for (Pacific by default).

Cards are collected from the recorders every week or two, so recent times often have no data yet. detection_summary's coverage lists the cards with recordings in your range, when each card's first and last recordings started, and whether BirdNET has finished with it -- read it before drawing conclusions from low counts.

Each detection has a review status: unreviewed (BirdNET's word alone), confirmed (a volunteer listened and agreed), or rejected (a volunteer listened and it wasn't that species). A reviewer's correction replaces BirdNET's species. Confidence is BirdNET's score, 0 to 1; low-confidence detections are often wrong.

Start with list_recorders, then detection_summary for the span you want. Use detections_export when you need every row and can fetch a URL and process CSV in code.`

// mcpFilter is the span and filters both range tools take.
type mcpFilter struct {
	Since         string   `json:"since,omitempty" jsonschema:"the start of the range, inclusive, as an RFC 3339 timestamp with its offset, e.g. 2026-09-14T12:00:00-07:00. Defaults to 7 days before 'until'."`
	Until         string   `json:"until,omitempty" jsonschema:"the end of the range, exclusive, as an RFC 3339 timestamp with its offset. Defaults to now. At most 31 days after 'since'."`
	MinConfidence float64  `json:"minConfidence,omitempty" jsonschema:"only detections BirdNET scored at or above this, from 0 to 1. Defaults to 0: everything stored."`
	Recorders     []string `json:"recorders,omitempty" jsonschema:"recorder ids, from list_recorders, to include. Omit for every recorder."`
	TimeZone      string   `json:"timeZone,omitempty" jsonschema:"the IANA time zone the answer gives times in and counts days and hours by, e.g. America/Los_Angeles (the default)."`
}

// mcpRange is an mcpFilter read and checked. Its times are in its zone, so
// they marshal with that zone's offset.
type mcpRange struct {
	Since         time.Time `json:"since"`
	Until         time.Time `json:"until"`
	TimeZone      string    `json:"timeZone"`
	MinConfidence float64   `json:"minConfidence"`
	Recorders     []string  `json:"recorders,omitempty"`
}

// errMCP is a tool call that was asked for something it can't answer. Its
// message goes back to the model, which is meant to correct the call.
type errMCP struct{ msg string }

func (e errMCP) Error() string { return e.msg }

// resolve checks a filter against the clock and the recorders there are.
func (f mcpFilter) resolve(now time.Time, recorders []db.Recorder) (mcpRange, error) {
	zone := cmp.Or(f.TimeZone, defaultMCPZone)
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return mcpRange{}, errMCP{fmt.Sprintf("there is no time zone %q; name an IANA zone like America/Los_Angeles", f.TimeZone)}
	}
	until := now.Truncate(time.Second)
	if f.Until != "" {
		if until, err = time.Parse(time.RFC3339, f.Until); err != nil {
			return mcpRange{}, errMCP{"'until' must be an RFC 3339 timestamp with its offset, like 2026-09-21T12:00:00-07:00"}
		}
	}
	since := until.Add(-defaultMCPSpan)
	if f.Since != "" {
		if since, err = time.Parse(time.RFC3339, f.Since); err != nil {
			return mcpRange{}, errMCP{"'since' must be an RFC 3339 timestamp with its offset, like 2026-09-14T12:00:00-07:00"}
		}
	}
	switch {
	case !since.Before(until):
		return mcpRange{}, errMCP{"'since' must be before 'until'"}
	case until.Sub(since) > maxMCPSpan:
		return mcpRange{}, errMCP{fmt.Sprintf("that is %.1f days; ask for at most %d at a time", until.Sub(since).Hours()/24, int(maxMCPSpan.Hours()/24))}
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
		Since: since.In(loc), Until: until.In(loc), TimeZone: loc.String(),
		MinConfidence: f.MinConfidence, Recorders: picked,
	}, nil
}

// zone is the range's time zone; resolve checked it loads.
func (r mcpRange) zone() *time.Location {
	loc, _ := time.LoadLocation(r.TimeZone)
	return loc
}

// filter is the store query for a range.
func (r mcpRange) filter() db.DetectionFilter {
	return db.DetectionFilter{
		Since: r.Since, Until: r.Until, MinConfidence: r.MinConfidence,
		// BirdNET's alone: Perch is a second opinion stored beside it, and the
		// two together would count most birds twice (CLAUDE.md, *Perch*).
		Model:       db.ModelBirdNET,
		RecorderIDs: r.Recorders,
	}
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
		Description: "Every recorder, retired ones included: its id (which the other tools filter by), name, position, and when its most recent recording on any card -- and on a card BirdNET has finished with -- started. Call this first to learn the recorder ids.",
		Annotations: readOnly,
	}, h.toolListRecorders)
	mcp.AddTool(server, &mcp.Tool{
		Name:  "detection_summary",
		Title: "Summarize detections",
		Description: "Aggregates BirdNET's detections between two timestamps (at most 31 days apart; the last 7 days by default): totals; the cards with recordings in the range, per recorder, and whether BirdNET has finished with each; detections per day and recorder; and per species, its count, days, recorders, confidence, review counts, detections by hour of the day, and a link to its most confident detection, where a person can hear the clip. Days and hours are counted in timeZone. " +
			"Rejected detections are counted in totals.rejected and left out of everything else. To compare with an earlier period, call it again with that range.",
		Annotations: readOnly,
	}, h.toolDetectionSummary)
	mcp.AddTool(server, &mcp.Tool{
		Name:  "detections_export",
		Title: "Export detections as CSV",
		Description: "A link to every BirdNET detection between two timestamps as CSV, one row per detection, rejected ones included -- for analysis in code, when you can fetch a URL. It doesn't put the rows in this conversation: a week can be over 100,000 rows. " +
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
	// LatestRecordingAt is when its most recent recording on any card
	// started, and LatestAnalyzedRecordingAt the same on a card BirdNET has
	// finished with.
	LatestRecordingAt         *time.Time `json:"latestRecordingAt,omitempty"`
	LatestAnalyzedRecordingAt *time.Time `json:"latestAnalyzedRecordingAt,omitempty"`
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
	latest, analyzed := map[string]*time.Time{}, map[string]*time.Time{}
	later := func(into map[string]*time.Time, id string, t *time.Time) {
		if t != nil && (into[id] == nil || t.After(*into[id])) {
			into[id] = t
		}
	}
	for _, u := range uploads {
		if u.Status == db.StatusDeleting {
			continue
		}
		later(latest, u.RecorderID, u.LastRecordingAt)
		if analysisDone(u.Status) {
			later(analyzed, u.RecorderID, u.LastRecordingAt)
		}
	}
	out := recordersOut{Recorders: make([]RecorderInfo, len(recorders))}
	for i, r := range recorders {
		out.Recorders[i] = RecorderInfo{
			ID: r.ID, Name: r.Name, Latitude: r.Latitude, Longitude: r.Longitude,
			AddedOn: dateOf(r.CreatedAt), Retired: r.RetiredAt != nil,
			LatestRecordingAt: latest[r.ID], LatestAnalyzedRecordingAt: analyzed[r.ID],
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
	Totals   SummaryTotals      `json:"totals"`
	Coverage []RecorderCoverage `json:"coverage"`
	// Days is each recorder's count for each day it heard anything.
	Days    []DayTally       `json:"days"`
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

// RecorderCoverage is the cards a recorder has with recordings in the range.
// A recorder with none has no data there yet.
type RecorderCoverage struct {
	RecorderID   string         `json:"recorderId"`
	RecorderName string         `json:"recorderName"`
	Cards        []CardCoverage `json:"cards"`
}

// CardCoverage is one card's recordings. A card holds recordings from when
// its first one started to when its last one ended, one file's length after
// LastRecordingAt; between those, any gap is a recorder that didn't record.
type CardCoverage struct {
	Card             string    `json:"card"`
	FirstRecordingAt time.Time `json:"firstRecordingAt"`
	LastRecordingAt  time.Time `json:"lastRecordingAt"`
	// Analyzed is BirdNET having finished with the card; until it has, the
	// card's detections are still coming in.
	Analyzed bool `json:"analyzed"`
}

// DayTally is one recorder's day.
type DayTally struct {
	Date       string `json:"date"` // YYYY-MM-DD, in the range's time zone
	RecorderID string `json:"recorderId"`
	Detections int    `json:"detections"`
	Species    int    `json:"species"`
}

// SpeciesSummary is one species over the range.
type SpeciesSummary struct {
	ScientificName string `json:"scientificName"`
	CommonName     string `json:"commonName"`
	Detections     int    `json:"detections"`
	// Days is how many dates, in the range's time zone, it was heard on.
	Days int `json:"days"`
	// Recorders is its detections by recorder id.
	Recorders      map[string]int `json:"recorders"`
	Confirmed      int            `json:"confirmed"`
	Unreviewed     int            `json:"unreviewed"`
	MaxConfidence  float64        `json:"maxConfidence"`
	MeanConfidence float64        `json:"meanConfidence"`
	FirstHeard     time.Time      `json:"firstHeard"`
	LastHeard      time.Time      `json:"lastHeard"`
	// ByHour is its detections by the hour of the day they were heard, from
	// 0 (midnight to 1 a.m.) to 23, in the range's time zone.
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
		if !going[d.UploadID] {
			s.add(d)
		}
		return nil
	})
	if err != nil {
		return nil, SummaryOut{}, h.toolFailed("detection_summary", req, err)
	}
	out := s.result(coverage(rng, recorders, uploads))
	h.log.Info("MCP detection summary", "email", mcpUser(req), "since", rng.Since, "until", rng.Until,
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
	loc       *time.Location
	publicURL string
	totals    SummaryTotals
	species   map[string]*speciesTally
	// days counts by date and recorder, with the species heard in each.
	days map[[2]string]*dayTally
}

type speciesTally struct {
	SpeciesSummary
	days    map[string]bool
	confSum float64
}

type dayTally struct {
	detections int
	species    map[string]bool
}

func newSummary(rng mcpRange, publicURL string) *summary {
	return &summary{rng: rng, loc: rng.zone(), publicURL: publicURL, species: map[string]*speciesTally{}, days: map[[2]string]*dayTally{}}
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
			days:           map[string]bool{},
		}
		s.species[scientific] = t
	}
	heard := d.DetectedAt.In(s.loc)
	date := heard.Format(time.DateOnly)
	t.Detections++
	t.days[date] = true
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

	key := [2]string{date, d.RecorderID}
	n := s.days[key]
	if n == nil {
		n = &dayTally{species: map[string]bool{}}
		s.days[key] = n
	}
	n.detections++
	n.species[scientific] = true
}

func (s *summary) result(cov []RecorderCoverage) SummaryOut {
	out := SummaryOut{
		Range: s.rng, Totals: s.totals, Coverage: cov,
		Days: make([]DayTally, 0, len(s.days)), Species: make([]SpeciesSummary, 0, len(s.species)),
	}
	out.Totals.Species = len(s.species)
	for key, n := range s.days {
		out.Days = append(out.Days, DayTally{Date: key[0], RecorderID: key[1], Detections: n.detections, Species: len(n.species)})
	}
	slices.SortFunc(out.Days, func(a, b DayTally) int {
		return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.RecorderID, b.RecorderID))
	})
	for _, t := range s.species {
		t.Days = len(t.days)
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

// coverage is, per recorder, the cards with recordings in the range: those
// whose first recording started before the range ends and whose last started
// within a day before it begins -- a recording can't run longer than that,
// and a card whose last file began earlier holds nothing in range. The
// recorders are the ones asked for, or every recorder still in the field and
// any retired one with a card in range.
func coverage(rng mcpRange, recorders []db.Recorder, uploads []db.Upload) []RecorderCoverage {
	loc := rng.zone()
	cards := map[string][]CardCoverage{}
	for _, u := range uploads {
		if u.Status == db.StatusDeleting || !rng.includes(u.RecorderID) || u.FirstRecordingAt == nil || u.LastRecordingAt == nil {
			continue
		}
		if !u.FirstRecordingAt.Before(rng.Until) || u.LastRecordingAt.Before(rng.Since.Add(-24*time.Hour)) {
			continue
		}
		cards[u.RecorderID] = append(cards[u.RecorderID], CardCoverage{
			Card: u.ID, FirstRecordingAt: u.FirstRecordingAt.In(loc), LastRecordingAt: u.LastRecordingAt.In(loc),
			Analyzed: analysisDone(u.Status),
		})
	}
	out := []RecorderCoverage{}
	for _, r := range recorders {
		if !rng.includes(r.ID) || (len(rng.Recorders) == 0 && r.RetiredAt != nil && cards[r.ID] == nil) {
			continue
		}
		c := RecorderCoverage{RecorderID: r.ID, RecorderName: r.Name, Cards: cards[r.ID]}
		if c.Cards == nil {
			c.Cards = []CardCoverage{}
		}
		slices.SortFunc(c.Cards, func(a, b CardCoverage) int { return a.FirstRecordingAt.Compare(b.FirstRecordingAt) })
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
	"id", "card", "recorderId", "recorderName", "heardAt", "startSec", "endSec",
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
	name := exportName(rng)
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: fmt.Sprintf(
			"Every BirdNET detection from %s until %s is at the link below, as CSV with a header row (columns: %s). "+
				"Fetch it with an HTTP GET -- in code, e.g. curl -o %s '<url>' -- within 30 minutes; it needs no other credentials. "+
				"heardAt is in %s, with its offset. scientificName and commonName are after review; birdnet* are BirdNET's own label. "+
				"Rows are in no particular order.\n\n%s",
			rng.Since.Format(time.RFC3339), rng.Until.Format(time.RFC3339), strings.Join(exportColumns, ", "), name, rng.TimeZone, out.URL)},
		&mcp.ResourceLink{URI: out.URL, Name: name, Title: "Detections, " + rng.Since.Format(time.RFC3339) + " to " + rng.Until.Format(time.RFC3339), MIMEType: "text/csv"},
	}}
	h.log.Info("MCP export link issued", "email", mcpUser(req), "since", rng.Since, "until", rng.Until, "recorders", len(rng.Recorders))
	return res, out, nil
}

// exportName is the file an export saves as.
func exportName(rng mcpRange) string {
	stamp := func(t time.Time) string { return t.Format("20060102T1504") }
	return fmt.Sprintf("birdsense-detections-%s-%s.csv", stamp(rng.Since), stamp(rng.Until))
}

// serveMCPExport serves an export link: the range as CSV, streamed as it is
// read, so a week costs a page of memory rather than the week. The link is
// the credential (it is signed, says who asked for what, and lapses after
// exportLinkLife), because whatever fetches it -- code in a model's sandbox --
// holds no token. It still answers only while its person is on the roster.
func (h *handlers) serveMCPExport(w http.ResponseWriter, r *http.Request) {
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
	loc := rng.zone()
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
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, exportName(rng)))
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
		if going[d.UploadID] {
			return nil
		}
		scientific, common := d.Species()
		rows++
		out.Write([]string{
			d.ID, d.UploadID, d.RecorderID, cmp.Or(station[d.UploadID], d.RecorderID),
			d.DetectedAt.In(loc).Format(time.RFC3339),
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
	h.log.Info("MCP export served", "email", link.Email, "since", rng.Since, "until", rng.Until,
		"rows", rows, "took", time.Since(started).Round(time.Millisecond))
}
