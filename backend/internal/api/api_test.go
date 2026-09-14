package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	return newTestMuxMode(t, false)
}

func newTestMuxMode(t *testing.T, dev bool) *http.ServeMux {
	t.Helper()
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	Register(mux, store, slog.New(slog.NewTextHandler(io.Discard, nil)), dev)
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
	rec := do(t, newTestMux(t), http.MethodGet, "/api/v1/health", "", nil)
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

// The sign-in picker lists every address on the roster, so outside dev mode the
// route must not exist and the session must not advertise it.
func TestDevPeopleOnlyInDevMode(t *testing.T) {
	for _, dev := range []bool{false, true} {
		mux := newTestMuxMode(t, dev)

		rec := do(t, mux, http.MethodGet, "/api/v1/dev/people", "", nil)
		want := http.StatusNotFound
		if dev {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("dev=%v: GET /dev/people = %d, want %d", dev, rec.Code, want)
		}
		if dev {
			var body struct{ People []Person }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.People) == 0 {
				t.Errorf("dev people = %s, %v; want the roster", rec.Body, err)
			}
		}

		var session struct{ Dev bool }
		rec = do(t, mux, http.MethodGet, "/api/v1/session", "", nil)
		if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || session.Dev != dev {
			t.Errorf("dev=%v: GET /session = %s, %v", dev, rec.Body, err)
		}
	}
}

func TestPublicOverviewNeedsNoSession(t *testing.T) {
	rec := do(t, newTestMux(t), http.MethodGet, "/api/v1/public/overview?days=14", "", nil)
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
	mux := newTestMux(t)
	for _, path := range []string{"/api/v1/uploads", "/api/v1/stations"} {
		if rec := do(t, mux, http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous = %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestAdminRoutesRejectVolunteers(t *testing.T) {
	mux := newTestMux(t)
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
	mux := newTestMux(t)
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
	mux := newTestMux(t)
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
	mux := newTestMux(t)
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
	mux := newTestMux(t)
	vol := signedIn(t, mux, RoleVolunteer) // Jane
	// OWL-20260821-SR03 belongs to Marcus Lee.
	if rec := do(t, mux, http.MethodGet, "/api/v1/uploads/OWL-20260821-SR03", "", vol); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSignInRejectsAnAddressNotOnTheRoster(t *testing.T) {
	rec := do(t, newTestMux(t), http.MethodPost, "/api/v1/session", `{"email":"stranger@example.com"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAddPersonRejectsADuplicate(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	rec := do(t, mux, http.MethodPost, "/api/v1/admin/people",
		`{"name":"Jane Again","email":"jane@example.com","role":"volunteer"}`, admin)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func sessionFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestRosterEditsAreAdminOnly(t *testing.T) {
	mux := newTestMux(t)
	vol := signedIn(t, mux, RoleVolunteer)
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p3",
		`{"name":"Tomas","email":"tomas.reyes@example.com","role":"admin"}`, vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer PUT = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/p3", "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer DELETE = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestUpdatePerson(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p2",
		`{"name":"Jane Birder","email":"jane.birder@example.com","role":"admin"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	var body struct {
		Person Person `json:"person"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p := body.Person; p.ID != "p2" || p.Name != "Jane Birder" || p.Email != "jane.birder@example.com" || p.Role != RoleAdmin {
		t.Errorf("person = %+v", p)
	}

	// Her cards follow her to the new name and address.
	rec = do(t, mux, http.MethodPost, "/api/v1/session", `{"email":"jane.birder@example.com"}`, nil)
	jane := sessionFrom(rec)
	if rec.Code != http.StatusOK || jane == nil {
		t.Fatalf("sign in with the new address = %d (%s)", rec.Code, rec.Body)
	}
	var mine struct {
		Uploads []Upload `json:"uploads"`
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/uploads", "", jane)
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil || len(mine.Uploads) == 0 {
		t.Errorf("renamed volunteer's cards = %s, %v; want them kept", rec.Body, err)
	}
}

func TestUpdatePersonRejectsBadInput(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"name":"Jane","email":"ellen.park@example.com","role":"volunteer"}`, http.StatusConflict},
		{`{"name":"Jane","email":"jane@example.com","role":"owner"}`, http.StatusBadRequest},
		{`{"name":"Jane","email":"not an address","role":"volunteer"}`, http.StatusBadRequest},
	} {
		if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p2", tc.body, admin); rec.Code != tc.want {
			t.Errorf("PUT %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p99",
		`{"name":"Nobody","email":"nobody@example.com","role":"volunteer"}`, admin); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown id = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestRemovePerson(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/p6", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/p6", "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// The next person added doesn't inherit the removed person's id.
	rec := do(t, mux, http.MethodPost, "/api/v1/admin/people",
		`{"name":"Alex Rivera","email":"alex@example.com","role":"volunteer"}`, admin)
	var added struct {
		Person Person `json:"person"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil || added.Person.ID != "p7" {
		t.Errorf("added = %s, %v; want id p7", rec.Body, err)
	}
}

func TestAnAdminCannotRemoveThemselves(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin) // Dana, p1
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/people/p1", "", admin); rec.Code != http.StatusConflict {
		t.Errorf("DELETE self = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestTheRosterKeepsAnAdmin(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin) // Dana, p1; Ellen, p5, is the other admin
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p5",
		`{"name":"Ellen Park","email":"ellen.park@example.com","role":"volunteer"}`, admin); rec.Code != http.StatusOK {
		t.Fatalf("demote Ellen = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p1",
		`{"name":"Dana Coordinator","email":"dana@eastsideaudubon.org","role":"volunteer"}`, admin); rec.Code != http.StatusConflict {
		t.Errorf("demote the last admin = %d, want %d", rec.Code, http.StatusConflict)
	}

	// Removal can only reach the last admin through a race: two admins removing
	// each other at once. Whoever is second must lose.
	s := newStore()
	if err := s.RemovePerson("p5", "p1"); err != nil {
		t.Fatalf("Dana removes Ellen: %v", err)
	}
	if err := s.RemovePerson("p1", "p5"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("Ellen (already removed) removes Dana: err = %v, want ErrLastAdmin", err)
	}
}

func TestReaddressingYourselfKeepsYouSignedIn(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/people/p1",
		`{"name":"Dana Coordinator","email":"dana.c@eastsideaudubon.org","role":"admin"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	fresh := sessionFrom(rec)
	if fresh == nil {
		t.Fatal("editing your own address issued no new session cookie")
	}
	var session struct {
		User *Person `json:"user"`
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/session", "", fresh)
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil || session.User == nil || session.User.ID != "p1" {
		t.Errorf("session with the new cookie = %s, %v", rec.Body, err)
	}
}

func TestStationEditsAreAdminOnly(t *testing.T) {
	mux := newTestMux(t)
	vol := signedIn(t, mux, RoleVolunteer)
	if rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/SW-02",
		`{"name":"Marymoor","latitude":47.66,"longitude":-122.11}`, vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer PUT = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-02", "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer DELETE = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestUpdateStation(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/SW-02",
		`{"name":"  Marymoor Park – Dog Area ","latitude":47.661,"longitude":-122.115}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	var body struct {
		Station Station `json:"station"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st := body.Station; st.ID != "SW-02" || st.Name != "Marymoor Park – Dog Area" || st.Latitude != 47.661 || st.Longitude != -122.115 {
		t.Errorf("station = %+v", st)
	}

	// Cards already sent keep the name they were recorded under.
	var all struct {
		Uploads []Upload `json:"uploads"`
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/admin/uploads", "", admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, u := range all.Uploads {
		if u.StationID == "SW-02" && u.StationName != "Marymoor Park – Snag Row" {
			t.Errorf("card %s now says %q; want the name it was sent under", u.Reference, u.StationName)
		}
	}
}

func TestUpdateStationRejectsBadInput(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
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
	} {
		if rec := do(t, mux, http.MethodPut, "/api/v1/admin/stations/"+tc.id, tc.body, admin); rec.Code != tc.want {
			t.Errorf("PUT %s %s = %d, want %d", tc.id, tc.body, rec.Code, tc.want)
		}
	}
}

func TestRemoveStation(t *testing.T) {
	mux := newTestMux(t)
	admin := signedIn(t, mux, RoleAdmin)
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-05", "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/stations/SW-05", "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var list struct {
		Stations []Station `json:"stations"`
	}
	rec := do(t, mux, http.MethodGet, "/api/v1/stations", "", admin)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, st := range list.Stations {
		if st.ID == "SW-05" {
			t.Error("removed recorder is still listed")
		}
	}

	// And a new card can't be registered against it.
	vol := signedIn(t, mux, RoleVolunteer)
	rec = do(t, mux, http.MethodPost, "/api/v1/uploads",
		`{"stationId":"SW-05","pulledOn":"2026-09-14","nights":[{"date":"2026-09-12","files":24,"bytes":100}]}`, vol)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("card for a removed recorder = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	rec := do(t, newTestMux(t), http.MethodGet, "/api/v1/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q, want JSON", got)
	}
}
