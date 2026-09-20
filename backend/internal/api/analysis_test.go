package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/analysis"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/retention"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// recordingQueue is an analysis queue that only notes what it was told, and
// reports whatever status a test set on it.
type recordingQueue struct {
	mu     sync.Mutex
	refs   []string
	status analysis.Status
}

func (q *recordingQueue) Status() analysis.Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.status
}

func (q *recordingQueue) Enqueue(ref string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.refs = append(q.refs, ref)
}

func (q *recordingQueue) told() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.refs...)
}

type cardFilesBody struct {
	Upload Upload      `json:"upload"`
	Files  []AudioFile `json:"files"`
}

type detectionsBody struct {
	Detections []Detection `json:"detections"`
}

func TestTheLastFileQueuesTheCardForAnalysis(t *testing.T) {
	store, files := newTestStore(t), testFiles(t)
	queue := &recordingQueue{}
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: files, queue: queue, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: newKeyset(testSessionKey),
		now:  func() time.Time { return testNow },
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &tusServer{t: t, mux: mux, srv: srv, store: store, files: files}

	jane := signedIn(t, mux, db.RoleVolunteer)
	reg := s.register(jane, cardBody("SW-03", "2026-09-14", "", "2026-09-12", 2, 1))
	ref := reg.Upload.Reference
	s.send(jane, ref, reg.Files[0].Path, audio(1), 100)
	if told := queue.told(); len(told) != 0 {
		t.Errorf("queued %v with a file still to come", told)
	}
	s.send(jane, ref, reg.Files[1].Path, audio(2), 100)
	if told := queue.told(); len(told) != 1 || told[0] != ref {
		t.Errorf("queue was told %v, want [%s]", told, ref)
	}
}

// TestAdminCardPageShowsAudioRetention: a coordinator can see when a card's
// originals go, and which of its files have already lost theirs. The
// detections and clips those files produced are untouched, so the card page
// still shows what was heard in a file whose recording has gone.
func TestAdminCardPageShowsAudioRetention(t *testing.T) {
	store := newTestStore(t)
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: testFiles(t), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: newKeyset(testSessionKey), retention: retention.Policy{Window: 30 * 24 * time.Hour},
		now: func() time.Time { return testNow },
	})
	ctx := t.Context()
	ref := "OWL-20260913-SR05"
	received, gone := testNow.Add(-40*24*time.Hour), testNow.Add(-10*24*time.Hour)
	if _, err := store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		u.Status, u.ReceivedAt = db.StatusInReview, &received
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAudioFiles(ctx, ref, []db.AudioFile{
		{Path: "DATA/kept.wav", Night: "2026-09-12", SizeBytes: 100, Status: db.AudioAnalyzed, BlobName: "uploads/" + ref + "/kept"},
		{Path: "DATA/gone.wav", Night: "2026-09-12", SizeBytes: 100, Status: db.AudioAnalyzed, AudioDeletedAt: &gone},
	}); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	rec := do(t, mux, http.MethodGet, "/api/v1/admin/uploads/"+ref, "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET card = %d (%s)", rec.Code, rec.Body)
	}
	body := decodeInto[cardFilesBody](t, rec)
	// The card's audio hasn't all gone, so it still has a date to come.
	if at := body.Upload.AudioExpiresAt; at == nil || !at.Equal(received.Add(30*24*time.Hour)) {
		t.Errorf("audioExpiresAt = %v, want %v", at, received.Add(30*24*time.Hour))
	}
	if body.Upload.AudioDeletedAt != nil {
		t.Errorf("audioDeletedAt = %v, want nil while a recording is still stored", body.Upload.AudioDeletedAt)
	}
	for _, f := range body.Files {
		switch f.Path {
		case "DATA/kept.wav":
			if f.AudioDeletedAt != nil {
				t.Errorf("%s: audioDeletedAt = %v, want nil", f.Path, f.AudioDeletedAt)
			}
		case "DATA/gone.wav":
			if f.AudioDeletedAt == nil || !f.AudioDeletedAt.Equal(gone) {
				t.Errorf("%s: audioDeletedAt = %v, want %v", f.Path, f.AudioDeletedAt, gone)
			}
			if f.Status != db.AudioAnalyzed {
				t.Errorf("%s: status = %q, want it still analyzed", f.Path, f.Status)
			}
		default:
			t.Errorf("unexpected file %q", f.Path)
		}
	}

	// With retention off, a card says nothing about when its audio goes.
	plain, _ := newTestMux(t)
	rec = do(t, plain, http.MethodGet, "/api/v1/admin/uploads/"+ref, "", signedIn(t, plain, db.RoleAdmin))
	if at := decodeInto[cardFilesBody](t, rec).Upload.AudioExpiresAt; at != nil {
		t.Errorf("audioExpiresAt with retention off = %v, want nil", at)
	}
}

func TestAdminCardPageShowsEachFileAndWhatWasHeard(t *testing.T) {
	mux, store := newTestMux(t)
	ctx := t.Context()
	ref := "OWL-20260913-SR05"
	analyzed := testNow
	if err := store.UpsertAudioFiles(ctx, ref, []db.AudioFile{
		{Path: "DATA/b.wav", Night: "2026-09-12", SizeBytes: 100, Status: db.AudioAnalyzed, AnalyzedAt: &analyzed, DetectionCount: 1},
		{Path: "DATA/a.wav", Night: "2026-09-12", SizeBytes: 100, Status: db.AudioFailed, StatusDetail: "unreadable audio"},
		{Path: "DATA/old.wav", Night: "2026-09-11", SizeBytes: 100, Status: db.AudioFailed, StatusDetail: db.AudioDetailNotOnCard},
	}); err != nil {
		t.Fatal(err)
	}
	fileB := db.AudioFileID(ref, "DATA/b.wav")
	if err := store.UpsertDetections(ctx, ref, []db.Detection{{
		AudioFileID: fileB, DetectedAt: testNow, StartSec: 12, EndSec: 15,
		ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.91,
	}}); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	rec := do(t, mux, http.MethodGet, "/api/v1/admin/uploads/"+ref, "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET card = %d (%s)", rec.Code, rec.Body)
	}
	body := decodeInto[cardFilesBody](t, rec)
	if body.Upload.Reference != ref || len(body.Files) != 2 || body.Files[0].Path != "DATA/a.wav" ||
		body.Files[0].StatusDetail != "unreadable audio" || body.Files[1].ID != fileB || body.Files[1].DetectionCount != 1 {
		t.Errorf("card page = %s; want the two listed files, by path", rec.Body)
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/detections/"+ref+"?file="+fileB, "", admin)
	// The seeded program has other detections on this card, in other files.
	if dets := decodeInto[detectionsBody](t, rec).Detections; rec.Code != http.StatusOK || len(dets) != 1 ||
		dets[0].CommonName != "Barred Owl" || dets[0].StartSec != 12 || dets[0].ReviewStatus != db.ReviewUnreviewed {
		t.Errorf("file detections = %d %s; want the one barred owl", rec.Code, rec.Body)
	}

	for path, want := range map[string]int{
		"/api/v1/admin/uploads/OWL-20990101-SR01":            http.StatusNotFound,
		"/api/v1/detections/OWL-20990101-SR01":               http.StatusNotFound,
		"/api/v1/detections/" + ref + "?file=af_nope":        http.StatusNotFound,
		"/api/v1/detections/OWL-20260821-SR03?file=" + fileB: http.StatusNotFound,
	} {
		if rec := do(t, mux, http.MethodGet, path, "", admin); rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	// The card page is a coordinator's, but what was heard is anyone's to hear.
	vol := signedIn(t, mux, db.RoleVolunteer)
	if rec := do(t, mux, http.MethodGet, "/api/v1/admin/uploads/"+ref, "", vol); rec.Code != http.StatusForbidden {
		t.Errorf("volunteer GET card = %d, want 403", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/api/v1/detections/"+ref+"?file="+fileB, "", vol)
	if dets := decodeInto[detectionsBody](t, rec).Detections; rec.Code != http.StatusOK || len(dets) != 1 {
		t.Errorf("volunteer GET file detections = %d %s; want the one barred owl", rec.Code, rec.Body)
	}
	// Without a file, the whole card: seedProgram's five and this one.
	rec = do(t, mux, http.MethodGet, "/api/v1/detections/"+ref, "", vol)
	if dets := decodeInto[detectionsBody](t, rec).Detections; rec.Code != http.StatusOK || len(dets) != 6 {
		t.Errorf("card detections = %d, %d of them; want 6", rec.Code, len(dets))
	}
}

type detectionBody struct {
	Upload    Upload    `json:"upload"`
	File      AudioFile `json:"file"`
	Detection Detection `json:"detection"`
}

func TestADetectionCanBeHeardAndReviewed(t *testing.T) {
	store, files := newTestStore(t), testFiles(t)
	mux := muxFor(store, files, false)
	ctx := t.Context()
	ref := "OWL-20260913-SR05"
	if err := store.UpsertAudioFiles(ctx, ref, []db.AudioFile{
		{Path: "DATA/b.wav", Night: "2026-09-12", SizeBytes: 100, Status: db.AudioAnalyzed, DetectionCount: 2},
	}); err != nil {
		t.Fatal(err)
	}
	fileID := db.AudioFileID(ref, "DATA/b.wav")
	owl := db.Detection{
		ID: db.DetectionID(fileID, 12_000, "Strix varia"), AudioFileID: fileID, DetectedAt: testNow,
		StartSec: 12, EndSec: 21, ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.91,
	}
	owl.Clip = &db.Clip{BlobName: storage.ClipName(ref, owl.ID), StartSec: 11, EndSec: 22}
	// Analyzed before clips were cut.
	older := db.Detection{
		ID: db.DetectionID(fileID, 72_000, "Bubo virginianus"), AudioFileID: fileID, DetectedAt: testNow.Add(time.Minute),
		StartSec: 72, EndSec: 75, ScientificName: "Bubo virginianus", CommonName: "Great Horned Owl", Confidence: 0.4,
	}
	if err := store.UpsertDetections(ctx, ref, []db.Detection{owl, older}); err != nil {
		t.Fatal(err)
	}
	const wav = "RIFF$\x00\x00\x00WAVEfmt not much of a clip"
	if err := files.Put(ctx, owl.Clip.BlobName, strings.NewReader(wav)); err != nil {
		t.Fatal(err)
	}

	admin := signedIn(t, mux, db.RoleAdmin)
	base := "/api/v1/detections/" + ref + "/"
	rec := do(t, mux, http.MethodGet, base+owl.ID, "", admin)
	if body := decodeInto[detectionBody](t, rec); rec.Code != http.StatusOK || body.Upload.Reference != ref ||
		body.File.Path != "DATA/b.wav" || body.Detection.AudioFileID != fileID || body.Detection.EndSec != 21 ||
		body.Detection.Clip == nil || body.Detection.Clip.StartSec != 11 || body.Detection.Clip.EndSec != 22 ||
		body.Detection.ReviewStatus != db.ReviewUnreviewed || body.Detection.Review != nil {
		t.Errorf("GET detection = %d %s; want it with its card, file and clip", rec.Code, rec.Body)
	}

	// The clip, whole and by range: browsers fetch audio in ranges.
	rec = do(t, mux, http.MethodGet, base+owl.ID+"/clip", "", admin)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" || rec.Body.String() != wav {
		t.Errorf("GET clip = %d %s %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, base+owl.ID+"/clip", nil)
	req.AddCookie(admin)
	req.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	mux.ServeHTTP(ranged, req)
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "RIFF" {
		t.Errorf("GET clip bytes=0-3 = %d %q; want 206 RIFF", ranged.Code, ranged.Body)
	}

	// Confirm, then discard, then undo.
	dana := userByEmail(t, store, "dana@eastsideaudubon.org")
	for _, status := range []string{db.ReviewConfirmed, db.ReviewRejected, db.ReviewUnreviewed} {
		rec := do(t, mux, http.MethodPut, base+owl.ID+"/review", `{"status":"`+status+`"}`, admin)
		got := decodeInto[struct {
			Detection Detection `json:"detection"`
		}](t, rec).Detection
		reviewed := status != db.ReviewUnreviewed
		if rec.Code != http.StatusOK || got.ReviewStatus != status || (got.Review != nil) != reviewed ||
			(reviewed && (got.Review.By != "Dana Coordinator" || !got.Review.At.Equal(testNow))) {
			t.Errorf("PUT review %s = %d %s", status, rec.Code, rec.Body)
		}
		stored, err := store.GetDetection(ctx, ref, owl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ReviewStatus != status || (reviewed && stored.Review.UserID != dana.ID) || stored.Clip == nil {
			t.Errorf("after %s, stored = %+v", status, stored)
		}
	}

	other := "/api/v1/detections/OWL-20260821-SR03/" + owl.ID
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, base + older.ID + "/clip", "", http.StatusNotFound},
		{http.MethodGet, base + "det_nope", "", http.StatusNotFound},
		{http.MethodGet, other, "", http.StatusNotFound},
		{http.MethodGet, other + "/clip", "", http.StatusNotFound},
		{http.MethodPut, other + "/review", `{"status":"confirmed"}`, http.StatusNotFound},
		{http.MethodPut, base + owl.ID + "/review", `{"status":"maybe"}`, http.StatusBadRequest},
	} {
		if rec := do(t, mux, c.method, c.path, c.body, admin); rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.path, rec.Code, c.want, rec.Body)
		}
	}
	// Anyone signed in hears and reviews, on anyone's card: this one is Marcus's,
	// and Jane reviews it. Someone not signed in can do neither.
	jane := signedIn(t, mux, db.RoleVolunteer)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base + owl.ID, ""},
		{http.MethodGet, base + owl.ID + "/clip", ""},
		{http.MethodPut, base + owl.ID + "/review", `{"status":"confirmed"}`},
	} {
		if rec := do(t, mux, c.method, c.path, c.body, jane); rec.Code != http.StatusOK {
			t.Errorf("volunteer %s %s = %d, want 200 (%s)", c.method, c.path, rec.Code, rec.Body)
		}
		if rec := do(t, mux, c.method, c.path, c.body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if stored, err := store.GetDetection(ctx, ref, owl.ID); err != nil || stored.ReviewStatus != db.ReviewConfirmed ||
		stored.Review == nil || stored.Review.UserName != "Jane Volunteer" {
		t.Errorf("after Jane's review, stored = %+v (%v)", stored, err)
	}
}

// A card that isn't moving is only explained by the queue's own state, so the
// coordinator's routes carry it -- and the volunteer's, whose detail would
// name server-side paths they can't act on, doesn't.
func TestAStuckCardSaysWhyOnTheAdminRoutes(t *testing.T) {
	store, files := newTestStore(t), testFiles(t)
	since := testNow.Add(-2 * time.Hour)
	queue := &recordingQueue{status: analysis.Status{
		State: analysis.StateUnavailable, Detail: "no module named birdnet", Since: since,
	}}
	mux := http.NewServeMux()
	register(mux, &handlers{
		store: store, files: files, queue: queue, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		keys: newKeyset(testSessionKey),
		now:  func() time.Time { return testNow },
	})
	admin, volunteer := signedIn(t, mux, db.RoleAdmin), signedIn(t, mux, db.RoleVolunteer)

	// The liveness probe stays 200 -- BirdNET being absent is not a reason to
	// restart the container -- but says so, without the detail: no session
	// reaches this route.
	rec := do(t, mux, http.MethodGet, "/api/v1/health", "", nil)
	health := decodeInto[map[string]string](t, rec)
	if rec.Code != http.StatusOK || health["status"] != "ok" || health["queue"] != analysis.StateUnavailable {
		t.Errorf("health = %d %s, want 200 with the queue unavailable", rec.Code, rec.Body)
	}

	ref := db.UploadID("2026-09-07", "SW-02")
	for _, path := range []string{"/api/v1/admin/uploads", "/api/v1/admin/uploads/" + ref} {
		rec := do(t, mux, http.MethodGet, path, "", admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body)
		}
		got := decodeInto[struct {
			Queue QueueStatus `json:"queue"`
		}](t, rec).Queue
		if got.State != analysis.StateUnavailable || got.Detail != "no module named birdnet" {
			t.Errorf("GET %s queue = %+v, want why nothing is being analyzed", path, got)
		}
		if got.Since == nil || !got.Since.Equal(since) {
			t.Errorf("GET %s queue.since = %v, want %v", path, got.Since, since)
		}
	}

	rec = do(t, mux, http.MethodGet, "/api/v1/uploads", "", volunteer)
	if strings.Contains(rec.Body.String(), "birdnet") {
		t.Errorf("the volunteer's own cards carried the queue's detail: %s", rec.Body)
	}
}

// A server running no queue at all still answers for it, so the screens say
// cards will sit rather than showing nothing.
func TestNoQueueReportsItself(t *testing.T) {
	h := &handlers{now: func() time.Time { return testNow }}
	if s := h.queueStatus(); s.State != "off" || s.Since != nil {
		t.Errorf("queueStatus with no queue = %+v, want off", s)
	}
}
