package api

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// seedWeek adds a week of recording, the evenings of 2026-09-14 to
// 2026-09-19, to the test program: a card from SW-02 that BirdNET has
// finished with, one from SW-03 still being analyzed, and one from SW-05
// being deleted. The detections are chosen to sit on each edge the range
// tools have to get right.
func seedWeek(t *testing.T, s db.Store) {
	t.Helper()
	ctx := t.Context()
	// Recording from 20:00 Pacific on the first evening to 05:00 after the
	// last.
	card := func(pulledOn, recorderID, name, status, firstEvening string, evenings int) string {
		t.Helper()
		evening, _ := time.ParseInLocation(time.DateOnly, firstEvening, pacific)
		first, last := evening.Add(20*time.Hour), evening.AddDate(0, 0, evenings).Add(5*time.Hour)
		u := db.Upload{
			ID: db.UploadID(pulledOn, recorderID), RecorderID: recorderID,
			Recorder: db.RecorderSnapshot{Name: name}, PulledOn: pulledOn, Status: status,
			FirstRecordingAt: &first, LastRecordingAt: &last, FileCount: evenings * 9,
		}
		if _, err := s.CreateUpload(ctx, u); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	done := card("2026-09-20", "SW-02", "Marymoor – card name", db.StatusInReview, "2026-09-14", 6)
	analyzing := card("2026-09-20", "SW-03", "Bridle Trails – North", db.StatusProcessing, "2026-09-15", 5)
	going := card("2026-09-21", "SW-05", "Soaring Eagle", db.StatusDeleting, "2026-09-15", 2)

	n := 0
	det := func(recorder, at, scientific, common string, confidence float64, status string) db.Detection {
		t.Helper()
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		n++
		return db.Detection{
			AudioFileID: "af_week_" + string(rune('a'+n)), RecorderID: recorder, DetectedAt: when,
			ScientificName: scientific, CommonName: common, Confidence: confidence, ReviewStatus: status,
		}
	}
	corrected := det("SW-02", "2026-09-18T04:00:00Z", "Bubo virginianus", "Great Horned Owl", 0.6, db.ReviewConfirmed)
	corrected.Review = &db.Review{CorrectedScientificName: "Strix varia", CorrectedCommonName: "Barred Owl"}
	perch := det("SW-02", "2026-09-16T05:30:00Z", "Strix varia", "Barred Owl", 0.95, db.ReviewUnreviewed)
	perch.Model = db.ModelPerch
	for card, ds := range map[string][]db.Detection{
		done: {
			// 22:30 Pacific on the 15th, and 03:10 on the 16th.
			det("SW-02", "2026-09-16T05:30:00Z", "Strix varia", "Barred Owl", 0.91, db.ReviewUnreviewed),
			det("SW-02", "2026-09-16T10:10:00Z", "Strix varia", "Barred Owl", 0.55, db.ReviewConfirmed),
			// A reviewer said no.
			det("SW-02", "2026-09-17T08:00:00Z", "Strix varia", "Barred Owl", 0.80, db.ReviewRejected),
			det("SW-02", "2026-09-17T06:00:00Z", "Bubo virginianus", "Great Horned Owl", 0.40, db.ReviewUnreviewed),
			// 11:30 Pacific on the 20th: just inside a range to noon.
			det("SW-02", "2026-09-20T18:30:00Z", "Turdus migratorius", "American Robin", 0.70, db.ReviewUnreviewed),
			// 13:00 that day: out of it.
			det("SW-02", "2026-09-20T20:00:00Z", "Turdus migratorius", "American Robin", 0.70, db.ReviewUnreviewed),
			// 04:00 on the 14th: before a range from noon.
			det("SW-02", "2026-09-14T11:00:00Z", "Strix varia", "Barred Owl", 0.99, db.ReviewUnreviewed),
			corrected,
			perch,
		},
		analyzing: {
			det("SW-03", "2026-09-16T09:00:00Z", "Megascops kennicottii", "Western Screech-Owl", 0.75, db.ReviewUnreviewed),
		},
		going: {
			det("SW-05", "2026-09-16T06:00:00Z", "Strix varia", "Barred Owl", 0.9, db.ReviewUnreviewed),
		},
	} {
		if err := s.UpsertDetections(ctx, card, ds); err != nil {
			t.Fatal(err)
		}
	}
}

// toolHandlers is the handlers behind oauthServerFor's mux, for calling the
// tools directly.
func toolHandlers(t *testing.T) (*handlers, *time.Time) {
	t.Helper()
	store := newTestStore(t)
	seedWeek(t, store)
	clock := time.Date(2026, time.September, 25, 18, 0, 0, 0, time.UTC)
	keys := newKeyset(testSessionKey)
	h := &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: keys, now: func() time.Time { return clock }, oauth: newOAuthServer(testPublicURL, keys),
	}
	register(http.NewServeMux(), h)
	return h, &clock
}

func callAs(email string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: email}}}
}

// theWeek is the week of evenings 14th to 19th, as a caller would ask for it
// -- noon to noon, so each evening keeps the morning after it.
var theWeek = mcpFilter{Since: "2026-09-14T12:00:00-07:00", Until: "2026-09-20T12:00:00-07:00"}

func TestDetectionSummary(t *testing.T) {
	h, _ := toolHandlers(t)
	_, out, err := h.toolDetectionSummary(t.Context(), callAs("jane@example.com"), theWeek)
	if err != nil {
		t.Fatal(err)
	}

	// In range and not rejected: the barred owls of the 15th and 16th, the
	// corrected one, the horned owl, the robin just before noon, and the
	// screech-owl. Not the deleting card's, Perch's, or either one outside
	// the range.
	want := SummaryTotals{Detections: 6, Species: 4, Confirmed: 2, Unreviewed: 4, Rejected: 1}
	if out.Totals != want {
		t.Errorf("totals = %+v, want %+v", out.Totals, want)
	}
	if out.Range.Since.Format(time.RFC3339) != "2026-09-14T12:00:00-07:00" || out.Range.Until.Format(time.RFC3339) != "2026-09-20T12:00:00-07:00" ||
		out.Range.TimeZone != "America/Los_Angeles" {
		t.Errorf("range = %+v, want the instants asked for, in Pacific", out.Range)
	}

	var names []string
	for _, s := range out.Species {
		names = append(names, s.ScientificName)
	}
	if !slices.Equal(names, []string{"Strix varia", "Bubo virginianus", "Megascops kennicottii", "Turdus migratorius"}) {
		t.Fatalf("species = %v, want the most heard first, then by name", names)
	}
	owl := out.Species[0]
	if owl.Detections != 3 || owl.Days != 3 || owl.Recorders["SW-02"] != 3 || owl.Confirmed != 2 || owl.Unreviewed != 1 {
		t.Errorf("barred owl = %d detections on %d days, %v, %d confirmed, %d unreviewed; want 3 on 3, all SW-02, the correction counted",
			owl.Detections, owl.Days, owl.Recorders, owl.Confirmed, owl.Unreviewed)
	}
	if owl.MaxConfidence != 0.91 || owl.MeanConfidence != 0.687 {
		t.Errorf("barred owl confidence = max %v mean %v, want 0.91 and 0.687", owl.MaxConfidence, owl.MeanConfidence)
	}
	if owl.ByHour[22] != 1 || owl.ByHour[3] != 1 || owl.ByHour[21] != 1 {
		t.Errorf("barred owl by hour = %v, want 21:00, 22:00 and 03:00 Pacific", owl.ByHour)
	}
	if !strings.HasPrefix(owl.Best.URL, testPublicURL+"/app/detections/OWL-20260920-SR02/") || owl.Best.Confidence != 0.91 {
		t.Errorf("barred owl's best = %+v, want the 0.91 one's page in the app", owl.Best)
	}
	if got := owl.FirstHeard.Format(time.RFC3339); got != "2026-09-15T22:30:00-07:00" {
		t.Errorf("barred owl first heard %s, want 22:30 Pacific on the 15th", got)
	}

	// Calendar days, Pacific: the 16th has the owl at 03:10 and the horned
	// owl at 23:00.
	wantDays := []DayTally{
		{Date: "2026-09-15", RecorderID: "SW-02", Detections: 1, Species: 1},
		{Date: "2026-09-16", RecorderID: "SW-02", Detections: 2, Species: 2},
		{Date: "2026-09-16", RecorderID: "SW-03", Detections: 1, Species: 1},
		{Date: "2026-09-17", RecorderID: "SW-02", Detections: 1, Species: 1},
		{Date: "2026-09-20", RecorderID: "SW-02", Detections: 1, Species: 1},
	}
	if !slices.Equal(out.Days, wantDays) {
		t.Errorf("days = %+v, want %+v", out.Days, wantDays)
	}

	// Coverage is the cards with recordings in range: SW-02's, analyzed, and
	// SW-03's, still going. SW-05's only card in range is being deleted and
	// its last one ended days before, so it has none; retired SW-09 has none
	// and isn't listed.
	cov := map[string]RecorderCoverage{}
	for _, c := range out.Coverage {
		cov[c.RecorderID] = c
	}
	if len(cov) != 3 {
		t.Errorf("coverage = %+v, want SW-02, SW-03 and SW-05", out.Coverage)
	}
	if c := cov["SW-02"].Cards; len(c) != 1 || c[0].Card != "OWL-20260920-SR02" || !c[0].Analyzed ||
		c[0].FirstRecordingAt.Format(time.RFC3339) != "2026-09-14T20:00:00-07:00" || c[0].LastRecordingAt.Format(time.RFC3339) != "2026-09-20T05:00:00-07:00" {
		t.Errorf("SW-02 coverage = %+v, want its week's card, analyzed, 20:00 on the 14th to 05:00 on the 20th", c)
	}
	if c := cov["SW-03"].Cards; len(c) != 1 || c[0].Analyzed {
		t.Errorf("SW-03 coverage = %+v, want its card, still being analyzed", c)
	}
	if c := cov["SW-05"]; len(c.Cards) != 0 || c.RecorderName != "Soaring Eagle – West Loop" {
		t.Errorf("SW-05 coverage = %+v, want no cards, under its current name", c)
	}

	// Asked in UTC, the same detections fall on UTC's days and hours.
	utc := theWeek
	utc.TimeZone = "UTC"
	_, out, err = h.toolDetectionSummary(t.Context(), callAs("jane@example.com"), utc)
	if err != nil {
		t.Fatal(err)
	}
	owl = out.Species[0]
	if owl.Days != 2 || owl.ByHour[5] != 1 || owl.FirstHeard.Format(time.RFC3339) != "2026-09-16T05:30:00Z" ||
		out.Range.Since.Format(time.RFC3339) != "2026-09-14T19:00:00Z" {
		t.Errorf("in UTC, barred owl = %d days, by hour %v, first %s, range from %s; want UTC's", owl.Days, owl.ByHour, owl.FirstHeard, out.Range.Since)
	}
}

func TestDetectionSummaryFilters(t *testing.T) {
	h, _ := toolHandlers(t)
	ctx := t.Context()

	confident := theWeek
	confident.MinConfidence = 0.7
	_, out, err := h.toolDetectionSummary(ctx, callAs("jane@example.com"), confident)
	if err != nil {
		t.Fatal(err)
	}
	if out.Totals.Detections != 3 || out.Totals.Rejected != 1 {
		t.Errorf("at 0.7 or more = %+v, want the 0.91 owl, the robin and the screech-owl, and the rejected 0.8", out.Totals)
	}

	one := theWeek
	one.Recorders = []string{"SW-03", "SW-03"}
	_, out, err = h.toolDetectionSummary(ctx, callAs("jane@example.com"), one)
	if err != nil {
		t.Fatal(err)
	}
	if out.Totals.Detections != 1 || len(out.Coverage) != 1 || out.Coverage[0].RecorderID != "SW-03" || !slices.Equal(out.Range.Recorders, []string{"SW-03"}) {
		t.Errorf("SW-03 alone = %+v, coverage %+v, range %+v; want its one detection and only its coverage", out.Totals, out.Coverage, out.Range)
	}

	for _, tc := range []struct {
		name   string
		filter mcpFilter
		says   string
	}{
		{"an unknown recorder", mcpFilter{Recorders: []string{"SW-99"}}, "list_recorders"},
		{"a backwards range", mcpFilter{Since: "2026-09-19T00:00:00Z", Until: "2026-09-14T00:00:00Z"}, "before 'until'"},
		{"a long range", mcpFilter{Since: "2026-08-01T00:00:00Z", Until: "2026-09-19T00:00:00Z"}, "at most 31"},
		{"a date with no time", mcpFilter{Until: "2026-09-19"}, "RFC 3339"},
		{"a time with no offset", mcpFilter{Since: "2026-09-12T20:00:00"}, "RFC 3339"},
		{"a confidence over 1", mcpFilter{MinConfidence: 1.5}, "from 0 to 1"},
		{"an unknown zone", mcpFilter{TimeZone: "Mars/Olympus_Mons"}, "IANA"},
		{"the server's zone", mcpFilter{TimeZone: "Local"}, "IANA"},
	} {
		_, _, err := h.toolDetectionSummary(ctx, callAs("jane@example.com"), tc.filter)
		var refused errMCP
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("summary with %s = %v, want an error saying %q", tc.name, err, tc.says)
		}
	}
}

func TestMCPRangeDefaultsToTheLastWeek(t *testing.T) {
	now := time.Date(2026, time.September, 25, 17, 0, 0, 0, time.UTC)
	rng, err := mcpFilter{}.resolve(now, nil)
	if err != nil || !rng.Until.Equal(now) || !rng.Since.Equal(now.Add(-7*24*time.Hour)) ||
		rng.Until.Format(time.RFC3339) != "2026-09-25T10:00:00-07:00" {
		t.Errorf("default range = %+v, %v; want the seven days to now, written in Pacific", rng, err)
	}
	// Naming only the end takes the week before it.
	rng, _ = mcpFilter{Until: "2026-09-01T00:00:00Z"}.resolve(now, nil)
	if rng.Since.UTC().Format(time.RFC3339) != "2026-08-25T00:00:00Z" {
		t.Errorf("a range with only an end starts %s, want the week before it", rng.Since)
	}
}

func TestListRecorders(t *testing.T) {
	h, _ := toolHandlers(t)
	_, out, err := h.toolListRecorders(t.Context(), callAs("jane@example.com"), struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RecorderInfo{}
	for _, r := range out.Recorders {
		got[r.ID] = r
	}
	if len(got) != 4 || !got["SW-09"].Retired {
		t.Errorf("recorders = %+v, want all four, SW-09 marked retired", out.Recorders)
	}
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	// The deleting card doesn't count; SW-05's latest is its older card.
	if r := got["SW-02"]; at(r.LatestRecordingAt) != "2026-09-20T12:00:00Z" || at(r.LatestAnalyzedRecordingAt) != "2026-09-20T12:00:00Z" {
		t.Errorf("SW-02 = %+v, want its week's card as the latest", r)
	}
	if r := got["SW-03"]; at(r.LatestRecordingAt) != "2026-09-20T12:00:00Z" || at(r.LatestAnalyzedRecordingAt) != "2026-08-21T12:00:00Z" {
		t.Errorf("SW-03 = %+v, want the week still analyzing, and its August card the latest analyzed", r)
	}
	if r := got["SW-05"]; at(r.LatestRecordingAt) != "2026-09-13T12:00:00Z" {
		t.Errorf("SW-05 = %+v, want its September card, not the one being deleted", r)
	}
}

func TestDetectionsExport(t *testing.T) {
	h, clock := toolHandlers(t)
	mux := http.NewServeMux()
	register(mux, h)

	res, out, err := h.toolDetectionsExport(t.Context(), callAs("jane@example.com"), theWeek)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.URL, testPublicURL+exportPath+"?token=") || !out.ExpiresAt.Equal(clock.Add(exportLinkLife)) {
		t.Errorf("export = %+v, want a link that lasts %s", out, exportLinkLife)
	}
	if len(res.Content) != 2 || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, out.URL) || res.Content[1].(*mcp.ResourceLink).URI != out.URL {
		t.Errorf("export content = %+v, want the link in words and as a resource link", res.Content)
	}
	link := strings.TrimPrefix(out.URL, testPublicURL)

	fetch := func(gz bool) [][]string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, link, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
			t.Fatalf("GET the export = %d %s, want CSV (%s)", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
		var body io.Reader = rec.Body
		if gz {
			if rec.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("export asked for gzip came as %q", rec.Header().Get("Content-Encoding"))
			}
			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = zr
		}
		rows, err := csv.NewReader(body).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	rows := fetch(false)
	if !slices.Equal(rows[0], exportColumns) {
		t.Errorf("header = %v, want %v", rows[0], exportColumns)
	}
	// The summary's six and the rejected one.
	if len(rows) != 8 {
		t.Errorf("export = %d rows, want 7 and a header", len(rows)-1)
	}
	col := func(name string) int { return slices.Index(exportColumns, name) }
	byTime := map[string][]string{}
	for _, r := range rows[1:] {
		byTime[r[col("heardAt")]] = r
	}
	if r := byTime["2026-09-20T11:30:00-07:00"]; r == nil || r[col("recorderName")] != "Marymoor – card name" {
		t.Errorf("the robin before noon = %v, want it in range, named as its card has it", r)
	}
	if r := byTime["2026-09-17T21:00:00-07:00"]; r == nil || r[col("scientificName")] != "Strix varia" || r[col("birdnetScientificName")] != "Bubo virginianus" || r[col("reviewStatus")] != db.ReviewConfirmed {
		t.Errorf("the corrected owl = %v, want the correction and BirdNET's label side by side", r)
	}
	if r := byTime["2026-09-17T01:00:00-07:00"]; r == nil || r[col("reviewStatus")] != db.ReviewRejected {
		t.Errorf("the rejected owl = %v, want it in the export, marked rejected", r)
	}
	for _, r := range rows[1:] {
		if !strings.HasPrefix(r[col("url")], testPublicURL+"/app/detections/") {
			t.Errorf("row url = %q, want the detection's page", r[col("url")])
		}
	}
	if gz := fetch(true); len(gz) != len(rows) {
		t.Errorf("gzipped export = %d rows, want %d", len(gz), len(rows))
	}

	// The link is the credential, so it lapses, and it is only ever good
	// while its person is on the roster.
	if rec := do(t, mux, http.MethodGet, link+"x", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("an edited export link = %d, want 401", rec.Code)
	}
	jane := userByEmail(t, h.store, "jane@example.com")
	removed := *clock
	if _, err := h.store.UpdateUser(t.Context(), jane.ID, func(u *db.User) error { u.RemovedAt = &removed; return nil }); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, mux, http.MethodGet, link, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("an export link after its person was removed = %d, want 401", rec.Code)
	}
	*clock = clock.Add(exportLinkLife + time.Second)
	if rec := do(t, mux, http.MethodGet, link, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("an export link past its life = %d, want 401", rec.Code)
	}
}

// failingScan is a store whose range read fails part way.
type failingScan struct{ db.Store }

func (failingScan) EachDetection(_ context.Context, _ db.DetectionFilter, fn func(db.Detection) error) error {
	fn(db.Detection{ID: "det_1"})
	return errors.New("cosmos: connection reset")
}

// TestAnExportThatFailsPartWayIsAborted holds that a failed read shows as a
// failed download, not as a CSV that is merely short: the status has gone
// by then, so aborting the response is the only way left to say so.
func TestAnExportThatFailsPartWayIsAborted(t *testing.T) {
	h, _ := toolHandlers(t)
	_, out, err := h.toolDetectionsExport(t.Context(), callAs("jane@example.com"), theWeek)
	if err != nil {
		t.Fatal(err)
	}
	h.store = failingScan{h.store}
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("a failed export recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	h.serveMCPExport(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, strings.TrimPrefix(out.URL, testPublicURL), nil))
}

// TestMCPWithTheSDKClient connects the MCP SDK's own client to a real server
// the way claude.ai would: discover, register, authorize in a "browser" that
// presses Allow, swap the code, then call the tools.
func TestMCPWithTheSDKClient(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicURL := "http://" + listener.Addr().String()

	store := newTestStore(t)
	seedWeek(t, store)
	keys := newKeyset(testSessionKey)
	h := &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: keys, now: time.Now, oauth: newOAuthServer(publicURL, keys),
	}
	mux := http.NewServeMux()
	register(mux, h)
	server := httptest.NewUnstartedServer(mux)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	// The browser: follow the authorization request to the consent page,
	// press Allow as Jane, and hand the client what it is sent back with.
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	allow := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		resp, err := browser.Get(args.URL)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		consent, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || consent.Path != connectPage {
			return nil, errors.New("authorize didn't lead to the consent page: " + resp.Header.Get("Location"))
		}
		body, _ := json.Marshal(map[string]any{"request": consent.Query().Get("request"), "approve": true})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, publicURL+oauthAuthorizePath, strings.NewReader(string(body)))
		req.AddCookie(sessionAt("jane@example.com", time.Now()))
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var decided map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&decided); err != nil {
			return nil, err
		}
		back, err := url.Parse(decided["redirect"])
		if err != nil {
			return nil, err
		}
		q := back.Query()
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
	oauth, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "SDK test client", RedirectURIs: []string{"http://localhost:9/callback"},
			GrantTypes: []string{grantCode, grantRefresh}, ResponseTypes: []string{"code"},
		}},
		AuthorizationCodeFetcher: allow,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: publicURL + mcpPath, OAuthHandler: oauth}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s isn't marked read-only", tool.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"detection_summary", "detections_export", "list_recorders"}) {
		t.Errorf("tools = %v", names)
	}

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "detection_summary", Arguments: map[string]any{"since": theWeek.Since, "until": theWeek.Until, "recorders": []string{"SW-02"}},
	})
	if err != nil || res.IsError {
		t.Fatalf("detection_summary = %v, %+v", err, res)
	}
	var out SummaryOut
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Totals.Detections != 5 || out.Species[0].ScientificName != "Strix varia" {
		t.Errorf("summary over the SDK = %+v, want SW-02's five", out.Totals)
	}

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "detection_summary", Arguments: map[string]any{"recorders": []string{"nope"}}})
	if err != nil || !res.IsError {
		t.Errorf("a summary for an unknown recorder = %v, %+v; want a tool error the model can read", err, res)
	}

	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "detections_export", Arguments: map[string]any{"since": theWeek.Since, "until": theWeek.Until}})
	if err != nil || res.IsError {
		t.Fatalf("detections_export = %v, %+v", err, res)
	}
	var export ExportOut
	raw, _ = json.Marshal(res.StructuredContent)
	json.Unmarshal(raw, &export)
	resp, err := http.Get(export.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil || resp.StatusCode != http.StatusOK || len(rows) != 8 {
		t.Errorf("fetching the export link = %d, %d rows, %v; want 7 rows and a header", resp.StatusCode, len(rows), err)
	}
}
