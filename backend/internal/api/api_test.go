package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux
}

// signedIn returns a mux plus the session cookie for the first person with the
// given role, so a test can call the routes behind requireSession.
func signedIn(t *testing.T, mux *http.ServeMux, role string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"role":"`+role+`"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in as %s = %d, want 200 (%s)", role, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("sign in as %s set no session cookie", role)
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

func TestHealth(t *testing.T) {
	rec := do(t, newTestMux(), http.MethodGet, "/api/v1/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
}

func TestPublicOverviewNeedsNoSession(t *testing.T) {
	rec := do(t, newTestMux(), http.MethodGet, "/api/v1/public/overview?days=14", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		WindowDays int          `json:"windowDays"`
		Program    ProgramStats `json:"program"`
		Species    []Species    `json:"species"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.WindowDays != 14 {
		t.Errorf("windowDays = %d, want 14", body.WindowDays)
	}
	if len(body.Species) == 0 || body.Species[0].CommonName == "" {
		t.Error("want at least one named species")
	}
	if body.Program.Recorders == 0 {
		t.Error("want the program stats filled in")
	}
}

func TestVolunteerRoutesNeedASession(t *testing.T) {
	mux := newTestMux()
	for _, path := range []string{"/api/v1/uploads", "/api/v1/stations"} {
		if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestAdminRoutesRejectVolunteers(t *testing.T) {
	mux := newTestMux()
	vol := signedIn(t, mux, RoleVolunteer)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", vol); rec.Code != http.StatusForbidden {
		t.Fatalf("volunteer GET /admin/people = %d, want %d", rec.Code, http.StatusForbidden)
	}
	admin := signedIn(t, mux, RoleAdmin)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/people", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/people = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestUploadsAreScopedToTheVolunteer(t *testing.T) {
	mux := newTestMux()
	vol := signedIn(t, mux, RoleVolunteer)

	rec := do(t, mux, http.MethodGet, "/api/v1/uploads", "", vol)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var mine struct {
		Uploads []Upload `json:"uploads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mine.Uploads) == 0 {
		t.Fatal("want the signed-in volunteer to have cards")
	}
	for _, u := range mine.Uploads {
		if u.VolunteerName != "Jane Volunteer" {
			t.Errorf("volunteer sees %q's card %s", u.VolunteerName, u.Reference)
		}
	}

	admin := signedIn(t, mux, RoleAdmin)
	rec = do(t, mux, http.MethodGet, "/api/v1/admin/uploads", "", admin)
	var all struct {
		Uploads []Upload `json:"uploads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(all.Uploads) <= len(mine.Uploads) {
		t.Errorf("admin sees %d cards, volunteer sees %d; want more", len(all.Uploads), len(mine.Uploads))
	}
}

func TestCreateUploadThenReportProgress(t *testing.T) {
	mux := newTestMux()
	vol := signedIn(t, mux, RoleVolunteer)

	body := `{"stationId":"SW-03","pulledOn":"2026-09-14","notes":"clear night",
	          "nights":[{"date":"2026-09-12","files":24,"bytes":100},
	                    {"date":"2026-09-13","files":6,"bytes":50,"flag":"partial"}]}`
	rec := do(t, mux, http.MethodPost, "/api/v1/uploads", body, vol)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want %d (%s)", rec.Code, http.StatusCreated, rec.Body)
	}
	var created struct {
		Upload Upload `json:"upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u := created.Upload
	if u.Reference != "OWL-20260914-SR03" {
		t.Errorf("reference = %q, want OWL-20260914-SR03", u.Reference)
	}
	if u.FileCount != 30 || u.TotalBytes != 150 {
		t.Errorf("totals = %d files / %d bytes, want 30 / 150", u.FileCount, u.TotalBytes)
	}

	rec = do(t, mux, http.MethodPost, "/api/v1/uploads/"+u.Reference+"/progress",
		`{"filesUploaded":30,"bytesUploaded":150}`, vol)
	if rec.Code != http.StatusOK {
		t.Fatalf("progress = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A fully received card moves itself on to processing; the client never
	// gets to declare a card done.
	if created.Upload.Status != StatusProcessing {
		t.Errorf("status = %q, want %q", created.Upload.Status, StatusProcessing)
	}
}

func TestProgressOnlyMovesForward(t *testing.T) {
	mux := newTestMux()
	vol := signedIn(t, mux, RoleVolunteer)
	const ref = "OWL-20260907-SR02" // seeded at 214 of 336 files

	rec := do(t, mux, http.MethodPost, "/api/v1/uploads/"+ref+"/progress", `{"filesUploaded":9}`, vol)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	var body struct {
		Upload Upload `json:"upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Upload.FilesUploaded != 214 {
		t.Errorf("filesUploaded = %d, want it left at 214", body.Upload.FilesUploaded)
	}
}

func TestOneVolunteerCannotReadAnothersCard(t *testing.T) {
	mux := newTestMux()
	vol := signedIn(t, mux, RoleVolunteer) // Jane
	// OWL-20260821-SR03 belongs to Marcus Lee.
	if rec := do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260821-SR03", "", vol); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSignInRejectsAnAddressNotOnTheRoster(t *testing.T) {
	rec := do(t, newTestMux(), http.MethodPost, "/api/v1/session", `{"email":"stranger@example.com"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAddPersonRejectsADuplicate(t *testing.T) {
	mux := newTestMux()
	admin := signedIn(t, mux, RoleAdmin)
	rec := do(t, mux, http.MethodPost, "/api/v1/admin/people",
		`{"name":"Jane Again","email":"jane@example.com","role":"volunteer"}`, admin)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	rec := do(t, newTestMux(), http.MethodGet, "/api/v1/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want JSON", got)
	}
}

// stubDep is a Dependency whose check does whatever the test says.
type stubDep struct {
	name string
	err  error
}

func (s stubDep) Name() string                { return s.name }
func (s stubDep) Check(context.Context) error { return s.err }

func TestHealthReportsDependencies(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, slog.New(slog.NewTextHandler(io.Discard, nil)),
		stubDep{name: "cosmos", err: errors.New("dial tcp: connection refused")})

	rec := do(t, mux, http.MethodGet, "/api/v1/health", "", nil)
	// A dependency being down must not look like the server being down: the
	// frontend and the public page do not need one.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status       string `json:"status"`
		Dependencies map[string]struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want %q", body.Status, "degraded")
	}
	if got := body.Dependencies["cosmos"].Status; got != "error" {
		t.Errorf("cosmos status = %q, want %q", got, "error")
	}
	if !strings.Contains(body.Dependencies["cosmos"].Error, "connection refused") {
		t.Errorf("cosmos error = %q, want the underlying failure", body.Dependencies["cosmos"].Error)
	}
}
