package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// registeredBody is what POST /api/v1/uploads answers.
type registeredBody struct {
	Upload Upload     `json:"upload"`
	Files  []CardFile `json:"files"`
}

// cardBody is a POST /api/v1/uploads body for a card with nights of perNight
// files, 100 bytes each, the first night on firstNight.
func cardBody(stationID, pulledOn, notes, firstNight string, nights, perNight int) string {
	first, err := time.Parse("2006-01-02", firstNight)
	if err != nil {
		panic(err)
	}
	var ns []Night
	var fs []CardFile
	for i := range nights {
		date := first.AddDate(0, 0, i).Format("2006-01-02")
		ns = append(ns, Night{Date: date, Files: perNight, Bytes: int64(100 * perNight)})
		for j := range perNight {
			path := fmt.Sprintf("DATA/%s/%03d.WAV", strings.ReplaceAll(date, "-", ""), j)
			fs = append(fs, CardFile{Path: path, Bytes: 100, Night: date})
		}
	}
	b, _ := json.Marshal(map[string]any{"stationId": stationID, "pulledOn": pulledOn, "notes": notes, "nights": ns, "files": fs})
	return string(b)
}

// tusServer is the API on a real listener, so uploads go through tusd the way
// a browser's do, with the file storage it writes to.
type tusServer struct {
	t     *testing.T
	mux   *http.ServeMux
	srv   *httptest.Server
	store db.Store
	files storage.Store
}

func newTusServer(t *testing.T) *tusServer {
	t.Helper()
	store, files := newTestStore(t), testFiles(t)
	mux := muxFor(store, files, false)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &tusServer{t: t, mux: mux, srv: srv, store: store, files: files}
}

type tusReply struct {
	status int
	header http.Header
	body   string
}

func (s *tusServer) do(method, url string, body []byte, header map[string]string, cookie *http.Cookie) tusReply {
	s.t.Helper()
	if strings.HasPrefix(url, "/") {
		url = s.srv.URL + url
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := s.srv.Client().Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return tusReply{status: res.StatusCode, header: res.Header, body: string(b)}
}

// create starts an upload of one file on a card, as tus-js-client does.
func (s *tusServer) create(cookie *http.Cookie, reference, path string, size int) tusReply {
	s.t.Helper()
	meta := []string{}
	if reference != "" {
		meta = append(meta, "reference "+base64.StdEncoding.EncodeToString([]byte(reference)))
	}
	if path != "" {
		meta = append(meta, "path "+base64.StdEncoding.EncodeToString([]byte(path)))
	}
	return s.do(http.MethodPost, tusPath, nil, map[string]string{
		"Upload-Length":   strconv.Itoa(size),
		"Upload-Metadata": strings.Join(meta, ","),
	}, cookie)
}

func (s *tusServer) patch(cookie *http.Cookie, location string, offset int, chunk []byte) tusReply {
	s.t.Helper()
	return s.do(http.MethodPatch, location, chunk, map[string]string{
		"Content-Type":  "application/offset+octet-stream",
		"Upload-Offset": strconv.Itoa(offset),
	}, cookie)
}

// send uploads data in chunks of at most chunk bytes and returns the upload URL.
func (s *tusServer) send(cookie *http.Cookie, reference, path string, data []byte, chunk int) string {
	s.t.Helper()
	created := s.create(cookie, reference, path, len(data))
	if created.status != http.StatusCreated {
		s.t.Fatalf("create %s = %d (%s)", path, created.status, created.body)
	}
	location := created.header.Get("Location")
	for offset := 0; offset < len(data); offset += chunk {
		end := min(offset+chunk, len(data))
		if r := s.patch(cookie, location, offset, data[offset:end]); r.status != http.StatusNoContent {
			s.t.Fatalf("patch %s at %d = %d (%s)", path, offset, r.status, r.body)
		}
	}
	return location
}

func (s *tusServer) register(cookie *http.Cookie, body string) registeredBody {
	s.t.Helper()
	rec := do(s.t, s.mux, http.MethodPost, "/api/v1/uploads", body, cookie)
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("register = %d (%s)", rec.Code, rec.Body)
	}
	return decodeInto[registeredBody](s.t, rec)
}

func (s *tusServer) card(cookie *http.Cookie, ref string) Upload {
	s.t.Helper()
	return decodeInto[uploadBody](s.t, do(s.t, s.mux, http.MethodGet, "/api/v1/uploads/"+ref, "", cookie)).Upload
}

func audio(n int) []byte {
	return bytes.Repeat([]byte{byte(n)}, 100)
}

func TestUploadingEveryFileMovesACardToProcessing(t *testing.T) {
	s := newTusServer(t)
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 2, 1))
	ref := reg.Upload.Reference
	if len(reg.Files) != 2 || reg.Files[0].Status != db.AudioPending {
		t.Fatalf("registered files = %+v, want 2 pending", reg.Files)
	}
	first, second := reg.Files[0].Path, reg.Files[1].Path

	s.send(jane, ref, first, audio(1), 100)
	if u := s.card(jane, ref); u.FilesUploaded != 1 || u.BytesUploaded != 100 || u.Status != db.StatusInProgress {
		t.Errorf("after one file: %d files, %d bytes, %s; want 1, 100, in_progress", u.FilesUploaded, u.BytesUploaded, u.Status)
	}
	f, err := s.store.GetAudioFile(t.Context(), ref, db.AudioFileID(ref, first))
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != db.AudioUploaded || f.UploadedAt == nil || !strings.HasPrefix(f.BlobName, "uploads/"+ref+"/") {
		t.Errorf("received file = %+v; want uploaded, under uploads/%s/", f, ref)
	}
	r, err := s.files.Open(t.Context(), f.BlobName)
	if err != nil {
		t.Fatalf("open %s: %v", f.BlobName, err)
	}
	stored, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(stored, audio(1)) {
		t.Errorf("stored %d bytes that aren't the file sent", len(stored))
	}

	// The second file goes in two chunks, and asking part-way through says how
	// much landed, which is what a resume starts from.
	created := s.create(jane, ref, second, 100)
	location := created.header.Get("Location")
	if r := s.patch(jane, location, 0, audio(2)[:60]); r.status != http.StatusNoContent {
		t.Fatalf("first chunk = %d (%s)", r.status, r.body)
	}
	if head := s.do(http.MethodHead, location, nil, nil, jane); head.header.Get("Upload-Offset") != "60" {
		t.Errorf("offset after a 60-byte chunk = %q", head.header.Get("Upload-Offset"))
	}
	if u := s.card(jane, ref); u.FilesUploaded != 1 {
		t.Errorf("a half-sent file counts: %d files uploaded", u.FilesUploaded)
	}
	if r := s.patch(jane, location, 60, audio(2)[60:]); r.status != http.StatusNoContent {
		t.Fatalf("last chunk = %d (%s)", r.status, r.body)
	}

	u := s.card(jane, ref)
	if u.FilesUploaded != 2 || u.BytesUploaded != 200 || u.Status != db.StatusProcessing {
		t.Errorf("after both files: %d files, %d bytes, %s; want 2, 200, processing", u.FilesUploaded, u.BytesUploaded, u.Status)
	}
	if card, _ := s.store.GetUpload(t.Context(), ref); card.ReceivedAt == nil || !card.ReceivedAt.Equal(testNow) {
		t.Errorf("receivedAt = %v, want %v", card.ReceivedAt, testNow)
	}

	// Choosing the same card again finds it in, with nothing left to send.
	again := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 2, 1))
	if u := again.Upload; u.FilesUploaded != 2 || u.FileCount != 2 || u.Status != db.StatusProcessing {
		t.Errorf("same card again = %d of %d files, %s; want 2 of 2, processing", u.FilesUploaded, u.FileCount, u.Status)
	}
	// Another folder for the same recorder and pull date isn't that card.
	other := do(t, s.mux, http.MethodPost, "/api/v1/uploads", cardBody("SW-03", "2026-09-14", "", "2026-09-01", 1, 5), jane)
	if other.Code != http.StatusConflict {
		t.Errorf("a different card over a received one = %d (%s), want %d", other.Code, other.Body, http.StatusConflict)
	}
	if u := s.card(jane, ref); u.FilesUploaded != 2 || u.FileCount != 2 || u.Status != db.StatusProcessing {
		t.Errorf("received card after a refused re-register = %d of %d, %s", u.FilesUploaded, u.FileCount, u.Status)
	}
}

func TestTusRefusesFilesThatArentOnTheCard(t *testing.T) {
	s := newTusServer(t)
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 2, 1))
	ref, listed := reg.Upload.Reference, reg.Files[0].Path
	s.send(jane, ref, listed, audio(1), 100)
	marcus := signInAs(t, s.mux, "m.lee@example.com")

	cases := []struct {
		what   string
		cookie *http.Cookie
		ref    string
		path   string
		size   int
		want   int
	}{
		{"not signed in", nil, ref, reg.Files[1].Path, 100, http.StatusUnauthorized},
		{"no metadata", jane, "", "", 100, http.StatusBadRequest},
		{"a file not on the list", jane, ref, "DATA/20260912/999.WAV", 100, http.StatusBadRequest},
		{"the wrong size", jane, ref, reg.Files[1].Path, 99, http.StatusBadRequest},
		{"a file already in", jane, ref, listed, 100, http.StatusUnprocessableEntity},
		{"someone else's card", marcus, ref, reg.Files[1].Path, 100, http.StatusNotFound},
		{"a card that's been received", marcus, "OWL-20260821-SR03", "DATA/a.WAV", 100, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if r := s.create(tc.cookie, tc.ref, tc.path, tc.size); r.status != tc.want {
			t.Errorf("%s: create = %d (%s), want %d", tc.what, r.status, strings.TrimSpace(r.body), tc.want)
		}
	}
	if u := s.card(jane, ref); u.FilesUploaded != 1 {
		t.Errorf("refused uploads changed the count to %d", u.FilesUploaded)
	}
}

func TestTusUploadIsOnlyReachableThroughItsCard(t *testing.T) {
	s := newTusServer(t)
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 1, 1))
	created := s.create(jane, reg.Upload.Reference, reg.Files[0].Path, 100)
	location := created.header.Get("Location")
	if created.status != http.StatusCreated || !strings.HasSuffix(strings.Split(location, tusPath)[0], "127.0.0.1:"+strings.Split(s.srv.URL, ":")[2]) {
		t.Fatalf("create = %d at %q", created.status, location)
	}

	marcus := signInAs(t, s.mux, "m.lee@example.com")
	if r := s.do(http.MethodHead, location, nil, nil, marcus); r.status != http.StatusNotFound {
		t.Errorf("another volunteer's HEAD = %d, want 404", r.status)
	}
	if r := s.patch(marcus, location, 0, audio(9)); r.status != http.StatusNotFound {
		t.Errorf("another volunteer's PATCH = %d, want 404", r.status)
	}
	if r := s.do(http.MethodHead, location, nil, nil, jane); r.status != http.StatusOK || r.header.Get("Upload-Offset") != "0" {
		t.Errorf("owner's HEAD = %d, offset %q; want 200 at 0", r.status, r.header.Get("Upload-Offset"))
	}
	if r := s.do(http.MethodHead, location, nil, nil, signedIn(t, s.mux, db.RoleAdmin)); r.status != http.StatusOK {
		t.Errorf("admin's HEAD = %d, want 200", r.status)
	}
	// Stored audio isn't served back, and uploads aren't deleted through tus.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if r := s.do(method, location, nil, nil, jane); r.status != http.StatusMethodNotAllowed {
			t.Errorf("%s an upload = %d, want 405", method, r.status)
		}
	}
}

func TestReRegisteringACardKeepsTheFilesAlreadyIn(t *testing.T) {
	s := newTusServer(t)
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 3, 1))
	ref := reg.Upload.Reference
	s.send(jane, ref, reg.Files[0].Path, audio(1), 100)

	// The same card again: the file that's in stays in.
	again := s.register(jane, cardBody("SW-03", "2026-09-14", "second try", "2026-09-12", 3, 1))
	if again.Upload.FilesUploaded != 1 || again.Files[0].Status != db.AudioUploaded || again.Files[1].Status != db.AudioPending {
		t.Errorf("re-registered = %d uploaded, files %+v; want the first file kept", again.Upload.FilesUploaded, again.Files)
	}

	// A different folder that shares only the later nights: the first night's
	// file is off the list and no longer counts, and the new one is pending.
	moved := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-13", 3, 1))
	paths := []string{}
	for _, f := range moved.Files {
		paths = append(paths, f.Path)
	}
	if moved.Upload.FilesUploaded != 0 || len(moved.Files) != 3 || moved.Files[0].Path != reg.Files[1].Path {
		t.Errorf("after a different list = %d uploaded, files %v", moved.Upload.FilesUploaded, paths)
	}
	gone, err := s.store.GetAudioFile(t.Context(), ref, db.AudioFileID(ref, reg.Files[0].Path))
	if err != nil || gone.Status != db.AudioFailed || gone.BlobName == "" {
		t.Errorf("the unlisted file = %+v, %v; want failed, still pointing at what was stored", gone, err)
	}
	if r := s.create(jane, ref, reg.Files[0].Path, 100); r.status != http.StatusBadRequest {
		t.Errorf("uploading the unlisted file = %d, want 400", r.status)
	}
}

func TestDeleteUpload(t *testing.T) {
	s := newTusServer(t)
	ctx := t.Context()
	jane := signedIn(t, s.mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 2, 1))
	ref := reg.Upload.Reference
	s.send(jane, ref, reg.Files[0].Path, audio(1), 100)
	// The second file is only part way in.
	partial := s.create(jane, ref, reg.Files[1].Path, 100).header.Get("Location")
	if r := s.patch(jane, partial, 0, audio(2)[:40]); r.status != http.StatusNoContent {
		t.Fatalf("patch = %d (%s)", r.status, r.body)
	}
	stored, err := s.store.GetAudioFile(ctx, ref, db.AudioFileID(ref, reg.Files[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertDetections(ctx, ref, []db.Detection{{AudioFileID: stored.ID, ScientificName: "Strix varia", CommonName: "Barred Owl"}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/uploads/" + ref

	if rec := do(t, s.mux, http.MethodDelete, path, "", jane); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer DELETE = %d, want %d", rec.Code, http.StatusForbidden)
	}
	admin := signedIn(t, s.mux, db.RoleAdmin)
	if rec := do(t, s.mux, http.MethodDelete, path, "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, want %d (%s)", rec.Code, http.StatusOK, rec.Body)
	}

	if _, err := s.store.GetUpload(ctx, ref); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("card after delete: err = %v, want ErrNotFound", err)
	}
	files, _ := s.store.ListAudioFiles(ctx, ref)
	found, _ := s.store.ListDetections(ctx, db.DetectionFilter{UploadID: ref})
	if len(files) != 0 || len(found) != 0 {
		t.Errorf("left behind %d audio files and %d detections", len(files), len(found))
	}
	if _, err := s.files.Open(ctx, stored.BlobName); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("stored audio after delete: err = %v, want ErrNotFound", err)
	}
	if r := s.patch(jane, partial, 40, audio(2)[40:]); r.status != http.StatusNotFound {
		t.Errorf("carrying on with the unfinished file = %d, want 404", r.status)
	}
	for _, u := range decodeInto[uploadsBody](t, do(t, s.mux, http.MethodGet, "/api/v1/admin/uploads", "", admin)).Uploads {
		if u.Reference == ref {
			t.Error("deleted card is still listed")
		}
	}
	// Other cards keep their detections.
	if others, _ := s.store.ListDetections(ctx, db.DetectionFilter{UploadID: "OWL-20260913-SR05"}); len(others) != 5 {
		t.Errorf("another card has %d detections, want its 5", len(others))
	}

	if rec := do(t, s.mux, http.MethodDelete, path, "", admin); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE again = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Two references can share a storage prefix, and deleting one card mustn't
// take the other's audio.
func TestDeleteUploadLeavesAudioSharedWithAnotherCard(t *testing.T) {
	store, dir := newTestStore(t), t.TempDir()
	files, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := muxFor(store, files, false)
	ctx := t.Context()
	for _, ref := range []string{"OWL-20260914-SRA B", "OWL-20260914-SRA_B"} {
		if _, err := store.CreateUpload(ctx, db.Upload{ID: ref, Status: db.StatusInReview}); err != nil {
			t.Fatal(err)
		}
	}
	blob := storage.Name(storagePrefix("OWL-20260914-SRA_B") + "/x")
	local := filepath.Join(dir, filepath.FromSlash(blob))
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, audio(1), 0o644); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	if rec := do(t, mux, http.MethodDelete, "/api/v1/admin/uploads/"+url.PathEscape("OWL-20260914-SRA B"), "", admin); rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (%s)", rec.Code, rec.Body)
	}
	if _, err := store.GetUpload(ctx, "OWL-20260914-SRA B"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("card after delete: err = %v, want ErrNotFound", err)
	}
	if r, err := files.Open(ctx, blob); err != nil {
		t.Errorf("the other card's audio went: %v", err)
	} else {
		r.Close()
	}
}
