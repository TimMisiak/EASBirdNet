package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// testNow is the handlers' clock in every test: 11 a.m. Pacific.
var testNow = time.Date(2026, time.September, 14, 18, 0, 0, 0, time.UTC)

// testSessionKey signs the test server's cookies, so a test can mint a session
// without an identity provider.
const testSessionKey = "test session key"

func newTestMux(t *testing.T) (*http.ServeMux, db.Store) {
	t.Helper()
	return newTestMuxMode(t, false)
}

func newTestMuxMode(t *testing.T, dev bool) (*http.ServeMux, db.Store) {
	t.Helper()
	store := newTestStore(t)
	return muxFor(store, testFiles(t), dev), store
}

// newTestStore is a JSON-file database holding seedProgram.
func newTestStore(t *testing.T) db.Store {
	t.Helper()
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	seedProgram(t, store)
	return store
}

func testFiles(t *testing.T) storage.Store {
	t.Helper()
	files, err := storage.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatalf("open test file storage: %v", err)
	}
	return files
}

func muxFor(store db.Store, files storage.Store, dev bool) *http.ServeMux {
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: files, log: slog.New(slog.NewTextHandler(io.Discard, nil)), dev: dev,
		keys: newKeyset(testSessionKey),
		now:  func() time.Time { return testNow },
	})
	return mux
}

// seedProgram is a small program the tests can reason about exactly. It is
// deliberately not devseed's, which is dated relative to today.
//
// Sorted by name, the first admin is Dana and the first volunteer is Jane, so
// those are who signedIn picks.
func seedProgram(t *testing.T, s db.Store) {
	t.Helper()
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	users := map[string]db.User{}
	for _, u := range []db.User{
		{Name: "Dana Coordinator", Email: "dana@eastsideaudubon.org", Role: db.RoleAdmin,
			Identity: &db.Identity{Provider: "microsoft", Subject: "dana-sub"}},
		{Name: "Ellen Park", Email: "ellen.park@example.com", Role: db.RoleAdmin},
		{Name: "Jane Volunteer", Email: "jane@example.com", Role: db.RoleVolunteer},
		{Name: "Marcus Lee", Email: "m.lee@example.com", Role: db.RoleVolunteer},
	} {
		created, err := s.CreateUser(ctx, u)
		must(err)
		users[u.Email] = created
	}

	retired := testNow.Add(-24 * time.Hour)
	for _, r := range []db.Recorder{
		{ID: "SW-02", Name: "Marymoor Park – Snag Row", Latitude: 47.66021, Longitude: -122.11384},
		{ID: "SW-03", Name: "Bridle Trails – North", Latitude: 47.656, Longitude: -122.17},
		// Renamed since its last card came in; see the card below.
		{ID: "SW-05", Name: "Soaring Eagle – West Loop", Latitude: 47.63914, Longitude: -121.9964},
		{ID: "SW-09", Name: "Pulled Out", Latitude: 47.6, Longitude: -122.0, RetiredAt: &retired},
	} {
		_, err := s.CreateRecorder(ctx, r)
		must(err)
	}

	card := func(recorderID, stationName, email, pulledOn, status, firstNight string, nights, filesUploaded int) {
		t.Helper()
		first, err := time.Parse("2006-01-02", firstNight)
		must(err)
		u := db.Upload{
			ID: db.UploadID(pulledOn, recorderID), RecorderID: recorderID,
			Recorder: db.RecorderSnapshot{Name: stationName},
			UserID:   users[email].ID, UserName: users[email].Name,
			PulledOn: pulledOn, Status: status, StartedAt: testNow.AddDate(0, 0, -30),
		}
		for i := range nights {
			u.Nights = append(u.Nights, db.Night{Date: first.AddDate(0, 0, i).Format("2006-01-02"), Files: 24, Bytes: 2400})
		}
		u.FileCount, u.TotalBytes = 24*nights, int64(2400*nights)
		u.FilesUploaded, u.BytesUploaded = filesUploaded, int64(100*filesUploaded)
		_, err = s.CreateUpload(ctx, u)
		must(err)
	}
	card("SW-02", "Marymoor Park – Snag Row", "jane@example.com", "2026-09-07", db.StatusInterrupted, "2026-08-24", 14, 214)
	card("SW-02", "Marymoor Park – Snag Row", "jane@example.com", "2026-01-01", db.StatusResultsSent, "2025-12-30", 2, 48)
	card("SW-03", "Bridle Trails – North", "m.lee@example.com", "2026-08-21", db.StatusResultsSent, "2026-08-07", 14, 336)
	card("SW-05", "Soaring Eagle – East Loop", "m.lee@example.com", "2026-09-13", db.StatusNeedsAttention, "2026-09-10", 3, 72)

	review := &db.Review{UserID: users["dana@eastsideaudubon.org"].ID, UserName: "Dana Coordinator", At: testNow}
	det := func(night, at, scientific, common, status string) db.Detection {
		t.Helper()
		when, err := time.Parse(time.RFC3339, at)
		must(err)
		d := db.Detection{
			AudioFileID: "af_" + at, DetectedAt: when, Night: night,
			ScientificName: scientific, CommonName: common, Confidence: 0.9, ReviewStatus: status,
		}
		if status != db.ReviewUnreviewed {
			r := *review
			d.Review = &r
		}
		return d
	}
	corrected := det("2026-09-10", "2026-09-11T08:00:00Z", "Bubo virginianus", "Great Horned Owl", db.ReviewConfirmed)
	corrected.Review.CorrectedScientificName, corrected.Review.CorrectedCommonName = "Strix varia", "Barred Owl"
	must(s.UpsertDetections(ctx, "OWL-20260913-SR05", []db.Detection{
		det("2026-09-12", "2026-09-13T10:00:00Z", "Strix varia", "Barred Owl", db.ReviewConfirmed),
		det("2026-09-11", "2026-09-12T10:00:00Z", "Strix varia", "Barred Owl", db.ReviewConfirmed),
		corrected,
		det("2026-09-12", "2026-09-13T11:00:00Z", "Strix varia", "Barred Owl", db.ReviewRejected),
		det("2026-09-12", "2026-09-13T12:00:00Z", "Megascops kennicottii", "Western Screech-Owl", db.ReviewUnreviewed),
	}))
	must(s.UpsertDetections(ctx, "OWL-20260821-SR03", []db.Detection{
		det("2026-08-19", "2026-08-20T09:00:00Z", "Aegolius acadicus", "Northern Saw-whet Owl", db.ReviewConfirmed),
	}))
}

// signedIn returns the session cookie for the first person with the given
// role, so a test can call the routes behind requireSession. It signs one
// rather than asking a route for it: real sign-in needs an identity provider,
// and the development sign-in only exists in dev mode.
//
// seedProgram sorts so that the first admin is Dana and the first volunteer is
// Jane, which is who the routes would have picked.
func signedIn(t *testing.T, _ *http.ServeMux, role string) *http.Cookie {
	t.Helper()
	switch role {
	case db.RoleAdmin:
		return signInAs(t, nil, "dana@eastsideaudubon.org")
	case db.RoleVolunteer:
		return signInAs(t, nil, "jane@example.com")
	}
	t.Fatalf("no seeded person has the role %q", role)
	return nil
}

// signInAs is a session cookie for one address, as setSession would write it.
func signInAs(t *testing.T, _ *http.ServeMux, email string) *http.Cookie {
	t.Helper()
	return &http.Cookie{
		Name:  sessionCookie,
		Value: newKeyset(testSessionKey).sign(strings.ToLower(email), testNow.Add(sessionLife)),
	}
}

// devSignIn goes through the development sign-in route, which only a dev-mode
// mux has.
func devSignIn(t *testing.T, mux *http.ServeMux, body string) *http.Cookie {
	t.Helper()
	rec := do(t, mux, http.MethodPost, "/api/v1/session", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in %s = %d, want 200 (%s)", body, rec.Code, rec.Body)
	}
	c := sessionFrom(rec)
	if c == nil {
		t.Fatalf("sign in %s set no session cookie", body)
	}
	return c
}

func sessionFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// decodeInto unmarshals a response body, failing the test if it can't.
func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func userByEmail(t *testing.T, s db.Store, email string) db.User {
	t.Helper()
	u, err := s.GetUserByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	return u
}

type uploadsBody struct {
	Uploads []Upload `json:"uploads"`
}

type uploadBody struct {
	Upload Upload `json:"upload"`
}

type personBody struct {
	Person Person `json:"person"`
}

type peopleBody struct {
	People []Person `json:"people"`
}

type stationsBody struct {
	Stations []Station `json:"stations"`
}

func TestHealth(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := do(t, mux, http.MethodGet, "/api/v1/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := decodeInto[map[string]string](t, rec); body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

// The sign-in picker lists every address on the roster, so outside dev mode the
// route must not exist and the session must not advertise it.
func TestDevPeopleOnlyInDevMode(t *testing.T) {
	for _, dev := range []bool{false, true} {
		mux, _ := newTestMuxMode(t, dev)

		rec := do(t, mux, http.MethodGet, "/api/v1/dev/people", "", nil)
		want := http.StatusNotFound
		if dev {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("dev=%v: GET /dev/people = %d, want %d", dev, rec.Code, want)
		}
		if dev {
			if body := decodeInto[peopleBody](t, rec); len(body.People) != 4 {
				t.Errorf("dev people = %s; want the roster of 4", rec.Body)
			}
		}

		rec = do(t, mux, http.MethodGet, "/api/v1/session", "", nil)
		if session := decodeInto[struct{ Dev bool }](t, rec); session.Dev != dev {
			t.Errorf("dev=%v: GET /session = %s", dev, rec.Body)
		}
	}
}

func TestPublicOverview(t *testing.T) {
	mux, _ := newTestMux(t)
	type overviewBody struct {
		WindowDays int          `json:"windowDays"`
		Program    ProgramStats `json:"program"`
		Species    []Species    `json:"species"`
	}

	// No session needed.
	rec := do(t, mux, http.MethodGet, "/api/v1/public/overview", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	body := decodeInto[overviewBody](t, rec)
	if body.WindowDays != 7 {
		t.Errorf("windowDays = %d, want 7", body.WindowDays)
	}
	// Three recorders in the field (SW-09 is retired); 2026's recorder-nights
	// are 14 + 14 + 3, the December nights being last year; four confirmed
	// detections this year, the rejected and unreviewed ones not counted.
	if want := (ProgramStats{Recorders: 3, NightsRecorded: 31, ConfirmedDetections: 4}); body.Program != want {
		t.Errorf("program = %+v, want %+v", body.Program, want)
	}
	// The corrected Great Horned Owl counts as the Barred Owl it was confirmed
	// as, and the station is the name on the card, not the renamed recorder.
	want := Species{
		CommonName: "Barred Owl", ScientificName: "Strix varia", Detections: 3, Nights: 3,
		Stations:       []string{"Soaring Eagle – East Loop"},
		LastDetectedAt: time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC),
	}
	if len(body.Species) != 1 || !sameSpecies(body.Species[0], want) {
		t.Errorf("7-day species = %+v, want only %+v", body.Species, want)
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/public/overview?days=30", "", nil)
	body = decodeInto[overviewBody](t, rec)
	var names []string
	for _, s := range body.Species {
		names = append(names, s.CommonName)
	}
	if body.WindowDays != 30 || !slices.Equal(names, []string{"Barred Owl", "Northern Saw-whet Owl"}) {
		t.Errorf("30-day window = %d days, species %v; want Barred Owl then Northern Saw-whet Owl", body.WindowDays, names)
	}
}

// countingStore counts how many times the public overview scans the database.
// ListRecorders is the first read overview makes and nothing else calls it.
type countingStore struct {
	db.Store
	scans atomic.Int64
}

func (s *countingStore) ListRecorders(ctx context.Context) ([]db.Recorder, error) {
	s.scans.Add(1)
	return s.Store.ListRecorders(ctx)
}

// The landing page needs no session, and answering it reads every recorder,
// every card and every confirmed detection of the year. Losing the cache would
// show up as a bill and a busy replica, not on any screen.
func TestPublicOverviewIsScannedOncePerMinutePerWindow(t *testing.T) {
	store := &countingStore{Store: newTestStore(t)}
	now := testNow
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: newKeyset(testSessionKey),
		now:  func() time.Time { return now },
	})
	ask := func(query string) string {
		t.Helper()
		rec := do(t, mux, http.MethodGet, "/api/v1/public/overview"+query, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /public/overview%s = %d (%s)", query, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	first := ask("")
	if again := ask(""); again != first {
		t.Errorf("a second request in the same minute answered differently:\n%s\n%s", first, again)
	}
	if store.scans.Load() != 1 {
		t.Errorf("scans after two requests in one minute = %d, want 1", store.scans.Load())
	}

	// A different window is its own answer, not the cached one.
	if thirty := ask("?days=30"); thirty == first {
		t.Errorf("?days=30 answered with the 7-day answer: %s", thirty)
	}
	if store.scans.Load() != 2 {
		t.Errorf("scans after a second window = %d, want 2", store.scans.Load())
	}

	// An answer doesn't outlive the minute it is stamped with.
	now = testNow.Add(time.Minute)
	ask("")
	if store.scans.Load() != 3 {
		t.Errorf("scans after the minute rolled over = %d, want 3", store.scans.Load())
	}

	// A burst arriving on a cold cache -- which is what an anonymous flood
	// looks like -- is one scan the rest wait for, not one each.
	now = testNow.Add(2 * time.Minute)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, mux, http.MethodGet, "/api/v1/public/overview", "", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("GET /public/overview = %d (%s)", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	if store.scans.Load() != 4 {
		t.Errorf("scans after 20 at once on a cold cache = %d, want 4", store.scans.Load())
	}
}

func sameSpecies(a, b Species) bool {
	return a.CommonName == b.CommonName && a.ScientificName == b.ScientificName &&
		a.Detections == b.Detections && a.Nights == b.Nights &&
		slices.Equal(a.Stations, b.Stations) && a.LastDetectedAt.Equal(b.LastDetectedAt)
}

// brokenStore is a database that has gone away.
type brokenStore struct{ db.Store }

func (brokenStore) ListRecorders(context.Context) ([]db.Recorder, error) {
	return nil, errors.New("cosmos: connection refused")
}

func TestADatabaseFailureIsA500WithoutTheDetail(t *testing.T) {
	_, store := newTestMux(t)
	mux := muxFor(brokenStore{store}, testFiles(t), false)
	rec := do(t, mux, http.MethodGet, "/api/v1/public/overview", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "cosmos") {
		t.Errorf("body = %s; the database error leaked to the client", rec.Body)
	}
}

func TestVolunteerRoutesNeedASession(t *testing.T) {
	mux, _ := newTestMux(t)
	for _, path := range []string{"/api/v1/uploads", "/api/v1/stations", "/api/v1/detections"} {
		if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestAdminRoutesRejectVolunteers(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", vol); rec.Code != http.StatusForbidden {
		t.Fatalf("volunteer GET /admin/people = %d, want %d", rec.Code, http.StatusForbidden)
	}
	admin := signedIn(t, mux, db.RoleAdmin)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/people = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestSession(t *testing.T) {
	mux, store := newTestMux(t)

	// Addresses match case-insensitively.
	jane := signInAs(t, mux, "Jane@Example.com")
	rec := do(t, mux, http.MethodGet, "/api/v1/session", "", jane)
	if session := decodeInto[struct{ User *Person }](t, rec); session.User == nil || session.User.Name != "Jane Volunteer" {
		t.Errorf("session = %s, want Jane", rec.Body)
	}
	// Dana has signed in with Microsoft; Jane never has with a provider.
	admin := signedIn(t, mux, db.RoleAdmin)
	rec = do(t, mux, http.MethodGet, "/api/v1/admin/people", "", admin)
	for _, p := range decodeInto[peopleBody](t, rec).People {
		if want := map[string]string{"Dana Coordinator": "Microsoft", "Jane Volunteer": "—"}[p.Name]; want != "" && p.Provider != want {
			t.Errorf("%s's provider = %q, want %q", p.Name, p.Provider, want)
		}
	}

	// Once Jane is removed, her open session ends.
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/"+userByEmail(t, store, "jane@example.com").ID, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("remove Jane = %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, mux, http.MethodGet, "/api/v1/uploads", "", jane); rec.Code != http.StatusUnauthorized {
		t.Errorf("removed person's session = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// With no identity provider configured -- dev mode's normal state -- the
// session still reports an empty list rather than null, because the sign-in
// page filters it to decide which provider buttons to draw.
func TestSessionProvidersIsAlwaysAList(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := do(t, mux, http.MethodGet, "/api/v1/session", "", nil)
	if !strings.Contains(rec.Body.String(), `"providers":[]`) {
		t.Errorf("session = %s, want an empty providers list", rec.Body)
	}
}

// The session cookie is signed, so the address in it is the server's word and
// not the browser's. This is the whole difference from the placeholder cookie,
// which anyone could type an admin's address into.
func TestSessionCookieIsSigned(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signInAs(t, mux, "dana@eastsideaudubon.org")

	for _, c := range []struct {
		what   string
		cookie *http.Cookie
	}{
		{"the bare address", &http.Cookie{Name: sessionCookie, Value: "dana@eastsideaudubon.org"}},
		{"another address spliced onto a real signature", &http.Cookie{
			Name:  sessionCookie,
			Value: forge(newKeyset(testSessionKey).sign("jane@example.com", testNow.Add(sessionLife)), admin.Value),
		}},
		{"a signature from another key", &http.Cookie{
			Name:  sessionCookie,
			Value: newKeyset("not the server's key").sign("dana@eastsideaudubon.org", testNow.Add(time.Hour)),
		}},
		{"an expired session", &http.Cookie{
			Name:  sessionCookie,
			Value: newKeyset(testSessionKey).sign("dana@eastsideaudubon.org", testNow.Add(-time.Second)),
		}},
	} {
		if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", c.cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want %d", c.what, rec.Code, http.StatusUnauthorized)
		}
	}

	// The real one still works, so the test isn't passing by rejecting everything.
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", admin); rec.Code != http.StatusOK {
		t.Errorf("a signed session = %d, want %d", rec.Code, http.StatusOK)
	}
}

// forge puts the body of one cookie under the signature of another, which is
// what tampering with a signed cookie actually looks like.
func forge(body, signature string) string {
	value, _, _ := reverseCut(body)
	_, mac, _ := reverseCut(signature)
	return value + "." + mac
}

// Signing in as anyone on the roster is the development picker. A deployed
// server must not offer it: OIDC is the only way in there.
func TestPlaceholderSignInIsDevOnly(t *testing.T) {
	prod, _ := newTestMuxMode(t, false)
	if rec := do(t, prod, http.MethodPost, "/api/v1/session", `{"role":"admin"}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /session outside dev = %d, want %d (%s)", rec.Code, http.StatusNotFound, rec.Body)
	}

	mux, store := newTestMuxMode(t, true)
	if rec := do(t, mux, http.MethodPost, "/api/v1/session", `{"email":"stranger@example.com"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("stranger = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// Signing in is recorded, and the cookie it sets is the signed one.
	jane := devSignIn(t, mux, `{"email":"Jane@Example.com"}`)
	if u := userByEmail(t, store, "jane@example.com"); u.LastSignInAt == nil || !u.LastSignInAt.Equal(testNow) {
		t.Errorf("lastSignInAt = %v, want %v", u.LastSignInAt, testNow)
	}
	rec := do(t, mux, http.MethodGet, "/api/v1/session", "", jane)
	if session := decodeInto[struct{ User *Person }](t, rec); session.User == nil || session.User.Name != "Jane Volunteer" {
		t.Errorf("session = %s, want Jane", rec.Body)
	}

	// A removed person can't sign in with it either.
	admin := devSignIn(t, mux, `{"role":"admin"}`)
	id := userByEmail(t, store, "jane@example.com").ID
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/"+id, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("remove Jane = %d (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, mux, http.MethodPost, "/api/v1/session", `{"email":"jane@example.com"}`, nil); rec.Code != http.StatusForbidden {
		t.Errorf("removed person signs in = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestUploadsAreScopedToTheVolunteer(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)

	rec := do(t, mux, http.MethodGet, "/api/v1/uploads", "", vol)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	mine := decodeInto[uploadsBody](t, rec).Uploads
	if len(mine) != 2 {
		t.Fatalf("Jane sees %d cards, want her 2", len(mine))
	}
	for _, u := range mine {
		if u.VolunteerName != "Jane Volunteer" {
			t.Errorf("volunteer sees %q's card %s", u.VolunteerName, u.Reference)
		}
	}
	if mine[0].Reference != "OWL-20260907-SR02" || len(mine[0].Nights) != 14 {
		t.Errorf("newest card = %+v, want OWL-20260907-SR02 with its 14 nights", mine[0])
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	rec = do(t, mux, http.MethodGet, "/api/v1/admin/uploads", "", admin)
	if all := decodeInto[uploadsBody](t, rec).Uploads; len(all) != 4 {
		t.Errorf("admin sees %d cards, want all 4", len(all))
	}
}

func TestCreateUpload(t *testing.T) {
	mux, store := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)

	body := `{"stationId":"SW-03","pulledOn":"2026-09-14","notes":" clear night ",
	          "nights":[{"date":"2026-09-12","files":2,"bytes":100},
	                    {"date":"2026-09-13","files":1,"bytes":50,"flag":"partial"}],
	          "files":[{"path":"DATA\\20260912\\a.WAV","bytes":60,"night":"2026-09-12"},
	                   {"path":"/DATA/20260912/b.WAV","bytes":40,"night":"2026-09-12"},
	                   {"path":"DATA/20260913/c.WAV","bytes":50,"night":"2026-09-13"}]}`
	rec := do(t, mux, http.MethodPost, "/api/v1/uploads", body, vol)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body)
	}
	reg := decodeInto[registeredBody](t, rec)
	u := reg.Upload
	if u.Reference != "OWL-20260914-SR03" || u.StationName != "Bridle Trails – North" || u.Notes != "clear night" {
		t.Errorf("upload = %+v", u)
	}
	if u.FileCount != 3 || u.TotalBytes != 150 || u.FilesUploaded != 0 || u.Status != db.StatusInProgress {
		t.Errorf("totals = %d files / %d bytes, %d in, %s; want 3 / 150, none in, in_progress", u.FileCount, u.TotalBytes, u.FilesUploaded, u.Status)
	}
	if len(reg.Files) != 3 || reg.Files[0].Path != "DATA/20260912/a.WAV" || reg.Files[1].Path != "DATA/20260912/b.WAV" || reg.Files[2].Status != db.AudioPending {
		t.Errorf("files = %+v; want the three, card paths normalized, pending", reg.Files)
	}

	stored, err := store.GetUpload(t.Context(), u.Reference)
	if err != nil {
		t.Fatal(err)
	}
	jane := userByEmail(t, store, "jane@example.com")
	if stored.UserID != jane.ID || stored.Recorder.Latitude != 47.656 || stored.ReceivedAt != nil {
		t.Errorf("stored card = %+v; want Jane's, with the recorder copied and nothing received", stored)
	}
	files, _ := store.ListAudioFiles(t.Context(), u.Reference)
	if len(files) != 3 || files[2].RecorderID != "SW-03" || files[2].SizeBytes != 50 || files[2].Night != "2026-09-13" {
		t.Errorf("stored files = %+v", files)
	}
}

func TestCreateUploadRejectsBadInput(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)
	const nights = `"nights":[{"date":"2026-09-12","files":2,"bytes":100}]`
	const files = `"files":[{"path":"a.WAV","bytes":60,"night":"2026-09-12"},{"path":"b.WAV","bytes":40,"night":"2026-09-12"}]`
	for _, body := range []string{
		`{"stationId":"SW-99","pulledOn":"2026-09-14",` + nights + `,` + files + `}`,
		`{"stationId":"SW-09","pulledOn":"2026-09-14",` + nights + `,` + files + `}`,
		`{"stationId":"SW-03","pulledOn":"14/09/2026",` + nights + `,` + files + `}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14","nights":[],` + files + `}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14",` + nights + `}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14","nights":[{"date":"2026-09-12","files":-1,"bytes":100}],` + files + `}`,
		// The file list has to be the card the nights describe.
		`{"stationId":"SW-03","pulledOn":"2026-09-14",` + nights + `,"files":[{"path":"a.WAV","bytes":100,"night":"2026-09-12"}]}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14",` + nights + `,"files":[{"path":"a.WAV","bytes":60,"night":"2026-09-12"},{"path":"a.WAV","bytes":40,"night":"2026-09-12"}]}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14",` + nights + `,"files":[{"path":"a.WAV","bytes":60,"night":"Sep 12"},{"path":"b.WAV","bytes":40,"night":"2026-09-12"}]}`,
		`{"stationId":"SW-03","pulledOn":"2026-09-14",` + nights + `,"files":[{"path":"/","bytes":60,"night":"2026-09-12"},{"path":"b.WAV","bytes":40,"night":"2026-09-12"}]}`,
	} {
		if rec := do(t, mux, http.MethodPost, "/api/v1/uploads", body, vol); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestReRegisteringACardResumesIt(t *testing.T) {
	mux, _ := newTestMux(t)
	jane := signedIn(t, mux, db.RoleVolunteer)

	rec := do(t, mux, http.MethodPost, "/api/v1/uploads", cardBody("SW-02", "2026-09-07", "second try", "2026-08-24", 14, 24), jane)
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-register = %d (%s)", rec.Code, rec.Body)
	}
	// The seeded card says 214 files landed, but it has none stored, and the
	// count comes from what is stored.
	reg := decodeInto[registeredBody](t, rec)
	if u := reg.Upload; u.Reference != "OWL-20260907-SR02" || u.FilesUploaded != 0 || u.Status != db.StatusInProgress || u.Notes != "second try" || len(reg.Files) != 336 {
		t.Errorf("resumed card = %+v with %d files; want its 336 files, none in, in_progress, the new notes", u, len(reg.Files))
	}
	// Picking the card up again answers the same list, so the browser can check
	// a folder against it before registering it.
	got := decodeInto[registeredBody](t, do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260907-SR02", "", jane))
	if len(got.Files) != len(reg.Files) || got.Files[0] != reg.Files[0] || got.Files[0].Status != db.AudioPending {
		t.Errorf("card's files = %d, first %+v; want the %d registered, pending", len(got.Files), got.Files[0], len(reg.Files))
	}

	// A finished card stays finished, with the list it was sent with. A
	// different list for its recorder and pull date is another card, and isn't
	// answered with this one's counts.
	rec = do(t, mux, http.MethodPost, "/api/v1/uploads", cardBody("SW-02", "2026-01-01", "", "2025-12-30", 1, 3), jane)
	if rec.Code != http.StatusConflict {
		t.Errorf("register a different card over a sent one = %d (%s), want %d", rec.Code, rec.Body, http.StatusConflict)
	}
	if u := decodeInto[uploadBody](t, do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260101-SR02", "", jane)).Upload; u.Status != db.StatusResultsSent || u.FileCount != 48 {
		t.Errorf("sent card after a refused re-register = %+v; want it left results_sent with 48 files", u)
	}

	// Someone else's card isn't theirs to take over.
	marcus := signInAs(t, mux, "m.lee@example.com")
	rec = do(t, mux, http.MethodPost, "/api/v1/uploads", cardBody("SW-02", "2026-09-07", "", "2026-08-24", 14, 24), marcus)
	if rec.Code != http.StatusConflict {
		t.Errorf("another volunteer registers Jane's card = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestProgressOnlySetsWhetherACardIsMoving(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)
	const ref = "OWL-20260907-SR02" // seeded interrupted, at 214 of 336 files

	rec := do(t, mux, http.MethodPost, "/api/v1/uploads/"+ref+"/progress", `{"status":"in_progress"}`, vol)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if u := decodeInto[uploadBody](t, rec).Upload; u.FilesUploaded != 214 || u.Status != db.StatusInProgress {
		t.Errorf("upload = %d files, %s; want 214 files, in_progress", u.FilesUploaded, u.Status)
	}
	// The server counts what lands; the browser can't say, or declare a card in.
	for _, body := range []string{`{"status":"processing"}`, `{"filesUploaded":336,"status":"in_progress"}`, `{}`} {
		if rec := do(t, mux, http.MethodPost, "/api/v1/uploads/"+ref+"/progress", body, vol); rec.Code != http.StatusBadRequest {
			t.Errorf("progress %s = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestProgressDoesNotReopenAFinishedCard(t *testing.T) {
	mux, _ := newTestMux(t)
	marcus := signInAs(t, mux, "m.lee@example.com")
	rec := do(t, mux, http.MethodPost, "/api/v1/uploads/OWL-20260821-SR03/progress", `{"status":"interrupted"}`, marcus)
	if u := decodeInto[uploadBody](t, rec).Upload; rec.Code != http.StatusOK || u.Status != db.StatusResultsSent {
		t.Errorf("progress on a sent card = %d, %+v; want it left results_sent", rec.Code, u)
	}
}

func TestOneVolunteerCannotReachAnothersCard(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer) // Jane
	// OWL-20260821-SR03 belongs to Marcus Lee.
	if rec := do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260821-SR03", "", vol); rec.Code != http.StatusNotFound {
		t.Errorf("GET = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if rec := do(t, mux, http.MethodPost, "/api/v1/uploads/OWL-20260821-SR03/progress", `{"status":"interrupted"}`, vol); rec.Code != http.StatusNotFound {
		t.Errorf("progress = %d, want %d", rec.Code, http.StatusNotFound)
	}
	admin := signedIn(t, mux, db.RoleAdmin)
	if rec := do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260821-SR03", "", admin); rec.Code != http.StatusOK {
		t.Errorf("admin GET = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAddPersonRejectsADuplicate(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	rec := do(t, mux, http.MethodPost, "/api/v1/admin/people",
		`{"name":"Jane Again","email":"JANE@example.com","role":"volunteer"}`, admin)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestRosterEditsAreAdminOnly(t *testing.T) {
	mux, store := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)
	path := "/api/v1/admin/people/" + userByEmail(t, store, "m.lee@example.com").ID
	if rec := do(t, mux, http.MethodPut, path, `{"name":"Marcus","email":"m.lee@example.com","role":"admin"}`, vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer PUT = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := do(t, mux, http.MethodDelete, path, "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer DELETE = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestUpdatePerson(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	id := userByEmail(t, store, "jane@example.com").ID
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/"+id,
		`{"name":"Jane Birder","email":"jane.birder@example.com","role":"admin"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if p := decodeInto[personBody](t, rec).Person; p.ID != id || p.Name != "Jane Birder" || p.Email != "jane.birder@example.com" || p.Role != db.RoleAdmin {
		t.Errorf("person = %+v", p)
	}

	// Her cards follow her to the new address, and keep the name they were
	// registered under.
	jane := signInAs(t, mux, "jane.birder@example.com")
	rec = do(t, mux, http.MethodGet, "/api/v1/uploads", "", jane)
	mine := decodeInto[uploadsBody](t, rec).Uploads
	if len(mine) != 2 || mine[0].VolunteerName != "Jane Volunteer" {
		t.Errorf("renamed volunteer's cards = %s; want her 2, under her old name", rec.Body)
	}
}

func TestUpdatePersonRejectsBadInput(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	path := "/api/v1/admin/people/" + userByEmail(t, store, "jane@example.com").ID
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"name":"Jane","email":"ellen.park@example.com","role":"volunteer"}`, http.StatusConflict},
		{`{"name":"Jane","email":"jane@example.com","role":"owner"}`, http.StatusBadRequest},
		{`{"name":"Jane","email":"not an address","role":"volunteer"}`, http.StatusBadRequest},
	} {
		if rec := do(t, mux, http.MethodPut, path, tc.body, admin); rec.Code != tc.want {
			t.Errorf("PUT %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/usr_0000000000000000",
		`{"name":"Nobody","email":"nobody@example.com","role":"volunteer"}`, admin); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown id = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// A removed person still holds their address, and the error says so.
	do(t, mux, http.MethodDelete, "/api/v1/admin/people/"+userByEmail(t, store, "m.lee@example.com").ID, "", admin)
	rec := do(t, mux, http.MethodPut, path, `{"name":"Jane","email":"m.lee@example.com","role":"volunteer"}`, admin)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "removed") {
		t.Errorf("take a removed person's address = %d %s; want 409 saying they were removed", rec.Code, rec.Body)
	}
}

func TestRemovePerson(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	marcus := userByEmail(t, store, "m.lee@example.com")
	path := "/api/v1/admin/people/" + marcus.ID

	if rec := do(t, mux, http.MethodDelete, path, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodDelete, path, "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if rec := do(t, mux, http.MethodPut, path, `{"name":"Marcus","email":"m.lee@example.com","role":"volunteer"}`, admin); rec.Code != http.StatusNotFound {
		t.Errorf("PUT a removed person = %d, want %d", rec.Code, http.StatusNotFound)
	}
	rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", admin)
	for _, p := range decodeInto[peopleBody](t, rec).People {
		if p.ID == marcus.ID {
			t.Error("removed person is still on the roster")
		}
	}

	// Nothing is hard-deleted: the document stays, and so do his cards.
	if u := userByEmail(t, store, "m.lee@example.com"); u.RemovedAt == nil || !u.RemovedAt.Equal(testNow) {
		t.Errorf("stored removedAt = %v, want %v", u.RemovedAt, testNow)
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260821-SR03", "", admin)
	if rec.Code != http.StatusOK {
		t.Errorf("removed person's card = %d, want %d", rec.Code, http.StatusOK)
	}

	// Adding the address again brings the same person back.
	rec = do(t, mux, http.MethodPost, "/api/v1/admin/people", `{"name":"Marcus A. Lee","email":"m.lee@example.com","role":"volunteer"}`, admin)
	if p := decodeInto[personBody](t, rec).Person; rec.Code != http.StatusCreated || p.ID != marcus.ID || p.Name != "Marcus A. Lee" {
		t.Errorf("re-add = %d %s; want 201 with id %s", rec.Code, rec.Body, marcus.ID)
	}
	if rec := do(t, mux, http.MethodPost, "/api/v1/admin/people", `{"email":"m.lee@example.com"}`, admin); rec.Code != http.StatusConflict {
		t.Errorf("re-add twice = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestAnAdminCannotRemoveThemselves(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin) // Dana
	path := "/api/v1/admin/people/" + userByEmail(t, store, "dana@eastsideaudubon.org").ID
	if rec := do(t, mux, http.MethodDelete, path, "", admin); rec.Code != http.StatusConflict {
		t.Errorf("DELETE self = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestTheRosterKeepsAnAdmin(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin) // Dana; Ellen is the other admin
	ellen := userByEmail(t, store, "ellen.park@example.com").ID
	dana := userByEmail(t, store, "dana@eastsideaudubon.org").ID
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/"+ellen,
		`{"name":"Ellen Park","email":"ellen.park@example.com","role":"volunteer"}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("demote Ellen = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/"+dana,
		`{"name":"Dana Coordinator","email":"dana@eastsideaudubon.org","role":"volunteer"}`, admin); rec.Code != http.StatusConflict {
		t.Errorf("demote the last admin = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestReaddressingYourselfKeepsYouSignedIn(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	dana := userByEmail(t, store, "dana@eastsideaudubon.org").ID
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/"+dana,
		`{"name":"Dana Coordinator","email":"dana.c@eastsideaudubon.org","role":"admin"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	fresh := sessionFrom(rec)
	if fresh == nil {
		t.Fatal("editing your own address issued no new session cookie")
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/session", "", fresh)
	if session := decodeInto[struct{ User *Person }](t, rec); session.User == nil || session.User.ID != dana {
		t.Errorf("session with the new cookie = %s", rec.Body)
	}
}

func TestStationEditsAreAdminOnly(t *testing.T) {
	mux, _ := newTestMux(t)
	vol := signedIn(t, mux, db.RoleVolunteer)
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/SW-02",
		`{"name":"Marymoor","latitude":47.66,"longitude":-122.11}`, vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer PUT = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-02", "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer DELETE = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestListStations(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := do(t, mux, http.MethodGet, "/api/v1/stations", "", signedIn(t, mux, db.RoleVolunteer))
	var ids []string
	for _, st := range decodeInto[stationsBody](t, rec).Stations {
		ids = append(ids, st.ID)
		if st.AddedOn != dateOf(time.Now()) {
			t.Errorf("%s addedOn = %q, want today (Pacific)", st.ID, st.AddedOn)
		}
	}
	if !slices.Equal(ids, []string{"SW-02", "SW-03", "SW-05"}) {
		t.Errorf("stations = %v, want the three in the field", ids)
	}
}

func TestAddStation(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	rec := do(t, mux, http.MethodPost, "/api/v1/admin/stations",
		`{"id":"SW-06","name":" Mercer Slough ","latitude":47.59,"longitude":-122.18}`, admin)
	if st := decodeInto[struct{ Station Station }](t, rec).Station; rec.Code != http.StatusCreated || st.ID != "SW-06" || st.Name != "Mercer Slough" {
		t.Errorf("add = %d %s", rec.Code, rec.Body)
	}
	// The unit labelled SW-02 is in the field whatever case it's typed in.
	rec = do(t, mux, http.MethodPost, "/api/v1/admin/stations",
		`{"id":"sw-02","name":"Marymoor","latitude":47.66,"longitude":-122.11}`, admin)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "SW-02") {
		t.Errorf("add sw-02 = %d %s; want 409 naming SW-02", rec.Code, rec.Body)
	}
}

// A recorder id is a Cosmos item id and partition key, a path segment in the
// card references built from it, and a blob-name prefix. An id the whitelist
// lets through would leave cards that can't be routed to or deleted.
func TestAddStationRejectsBadIDs(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	for _, tc := range []struct {
		id   string
		want int
	}{
		{"SW/06", http.StatusBadRequest},
		{`SW\06`, http.StatusBadRequest},
		{"SW?06", http.StatusBadRequest},
		{"SW#06", http.StatusBadRequest},
		{"SW 06", http.StatusBadRequest},
		{"-06", http.StatusBadRequest},
		{strings.Repeat("S", db.MaxRecorderIDLen+1), http.StatusBadRequest},
		// Both would make OWL-20260907-SR02 out of a card pulled from SW-02.
		{"02", http.StatusConflict},
		{"sw-02", http.StatusConflict},
	} {
		body := fmt.Sprintf(`{"id":%q,"name":"Mercer Slough","latitude":47.59,"longitude":-122.18}`, tc.id)
		if rec := do(t, mux, http.MethodPost, "/api/v1/admin/stations", body, admin); rec.Code != tc.want {
			t.Errorf("add %q = %d, want %d (%s)", tc.id, rec.Code, tc.want, rec.Body)
		}
	}
	// And a card can't be registered against an id no recorder could have.
	vol := signedIn(t, mux, db.RoleVolunteer)
	rec := do(t, mux, http.MethodPost, "/api/v1/uploads",
		`{"stationId":"SW/02","pulledOn":"2026-09-14","nights":[{"date":"2026-09-12","files":1,"bytes":100}],"files":[{"path":"a.wav","sizeBytes":100}]}`, vol)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("card for an unusable station id = %d, want %d (%s)", rec.Code, http.StatusBadRequest, rec.Body)
	}
}

func TestUpdateStation(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/SW-02",
		`{"name":"  Marymoor Park – Dog Area ","latitude":47.661,"longitude":-122.115}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if st := decodeInto[struct{ Station Station }](t, rec).Station; st.ID != "SW-02" || st.Name != "Marymoor Park – Dog Area" || st.Latitude != 47.661 || st.Longitude != -122.115 {
		t.Errorf("station = %+v", st)
	}

	// Cards already sent keep the name they were recorded under.
	rec = do(t, mux, http.MethodGet, "/api/v1/admin/uploads", "", admin)
	for _, u := range decodeInto[uploadsBody](t, rec).Uploads {
		if u.StationID == "SW-02" && u.StationName != "Marymoor Park – Snag Row" {
			t.Errorf("card %s now says %q; want the name it was sent under", u.Reference, u.StationName)
		}
	}
}

func TestUpdateStationRejectsBadInput(t *testing.T) {
	mux, _ := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	for _, tc := range []struct {
		id, body string
		want     int
	}{
		{"SW-02", `{"name":" ","latitude":47.6,"longitude":-122.1}`, http.StatusBadRequest},
		{"SW-02", `{"name":"Marymoor","latitude":0,"longitude":0}`, http.StatusBadRequest},
		{"SW-02", `{"name":"Marymoor","latitude":147.6,"longitude":-122.1}`, http.StatusBadRequest},
		// The id is printed on the unit; it isn't something an edit can change.
		{"SW-02", `{"id":"SW-09","name":"Marymoor","latitude":47.6,"longitude":-122.1}`, http.StatusBadRequest},
		{"SW-99", `{"name":"Nowhere","latitude":47.6,"longitude":-122.1}`, http.StatusNotFound},
		{"SW-09", `{"name":"Retired","latitude":47.6,"longitude":-122.1}`, http.StatusNotFound},
	} {
		if rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/"+tc.id, tc.body, admin); rec.Code != tc.want {
			t.Errorf("PUT %s %s = %d, want %d", tc.id, tc.body, rec.Code, tc.want)
		}
	}
}

// A coordinator who fills in one coordinate and not the other must not get a
// recorder at 0 -- Number("") is 0 in the browser, and the position goes to
// BirdNET's geo filter, which changes which species the model will report.
func TestStationRejectsHalfFilledPosition(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/admin/stations", `{"id":"SW-06","name":"Mercer Slough","latitude":47.59}`},
		{http.MethodPost, "/api/v1/admin/stations", `{"id":"SW-06","name":"Mercer Slough","latitude":47.59,"longitude":null}`},
		{http.MethodPost, "/api/v1/admin/stations", `{"id":"SW-06","name":"Mercer Slough","longitude":-122.18}`},
		{http.MethodPut, "/api/v1/admin/stations/SW-02", `{"name":"Marymoor","latitude":47.66}`},
		{http.MethodPut, "/api/v1/admin/stations/SW-02", `{"name":"Marymoor","longitude":-122.11}`},
	} {
		if rec := do(t, mux, tc.method, tc.path, tc.body, admin); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.body, rec.Code, http.StatusBadRequest, rec.Body)
		}
	}
	if _, err := store.GetRecorder(t.Context(), "SW-06"); err == nil {
		t.Error("a recorder was added with half a position")
	}
	if rec, err := store.GetRecorder(t.Context(), "SW-02"); err != nil || rec.Latitude != 47.66021 || rec.Longitude != -122.11384 {
		t.Errorf("SW-02 = %+v, %v; want its position untouched", rec, err)
	}
}

func TestRemoveStation(t *testing.T) {
	mux, store := newTestMux(t)
	admin := signedIn(t, mux, db.RoleAdmin)
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-05", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-05", "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}

	rec := do(t, mux, http.MethodGet, "/api/v1/stations", "", admin)
	for _, st := range decodeInto[stationsBody](t, rec).Stations {
		if st.ID == "SW-05" {
			t.Error("removed recorder is still listed")
		}
	}
	// It is retired, not deleted.
	if r, err := store.GetRecorder(t.Context(), "SW-05"); err != nil || r.RetiredAt == nil {
		t.Errorf("stored recorder = %+v, %v; want retiredAt set", r, err)
	}

	// A new card can't be registered against it.
	vol := signedIn(t, mux, db.RoleVolunteer)
	rec = do(t, mux, http.MethodPost, "/api/v1/uploads",
		`{"stationId":"SW-05","pulledOn":"2026-09-14","nights":[{"date":"2026-09-12","files":24,"bytes":100}]}`, vol)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("card for a removed recorder = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// Putting the unit back out brings the same recorder back.
	rec = do(t, mux, http.MethodPost, "/api/v1/admin/stations",
		`{"id":"sw-05","name":"Soaring Eagle – North","latitude":47.64,"longitude":-121.99}`, admin)
	if st := decodeInto[struct{ Station Station }](t, rec).Station; rec.Code != http.StatusCreated || st.ID != "SW-05" || st.Name != "Soaring Eagle – North" {
		t.Errorf("re-add = %d %s; want SW-05 back at its new name", rec.Code, rec.Body)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := do(t, mux, http.MethodGet, "/api/v1/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want JSON", got)
	}
}
