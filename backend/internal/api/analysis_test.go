package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// recordingQueue is an analysis queue that only notes what it was told.
type recordingQueue struct {
	mu   sync.Mutex
	refs []string
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
		now: func() time.Time { return testNow },
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

	rec = do(t, mux, http.MethodGet, "/api/v1/admin/uploads/"+ref+"/files/"+fileB+"/detections", "", admin)
	// The seeded program has other detections on this card, in other files.
	if dets := decodeInto[detectionsBody](t, rec).Detections; rec.Code != http.StatusOK || len(dets) != 1 ||
		dets[0].CommonName != "Barred Owl" || dets[0].StartSec != 12 || dets[0].ReviewStatus != db.ReviewUnreviewed {
		t.Errorf("file detections = %d %s; want the one barred owl", rec.Code, rec.Body)
	}

	for path, want := range map[string]int{
		"/api/v1/admin/uploads/OWL-20990101-SR01":                                http.StatusNotFound,
		"/api/v1/admin/uploads/" + ref + "/files/af_nope/detections":             http.StatusNotFound,
		"/api/v1/admin/uploads/OWL-20260821-SR03/files/" + fileB + "/detections": http.StatusNotFound,
	} {
		if rec := do(t, mux, http.MethodGet, path, "", admin); rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
	vol := signedIn(t, mux, db.RoleVolunteer)
	for _, path := range []string{"/api/v1/admin/uploads/" + ref, "/api/v1/admin/uploads/" + ref + "/files/" + fileB + "/detections"} {
		if rec := do(t, mux, http.MethodGet, path, "", vol); rec.Code != http.StatusForbidden {
			t.Errorf("volunteer GET %s = %d, want 403", path, rec.Code)
		}
	}
}
