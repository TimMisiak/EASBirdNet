package retention

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// testNow is the day a sweep runs in these tests. The cards are dated against
// it, so nothing here depends on the real clock.
var testNow = time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC)

const month = 30 * 24 * time.Hour

type fixture struct {
	t      *testing.T
	store  db.Store
	path   string
	files  storage.Store
	keeper *Sweeper
}

func newFixture(t *testing.T, window time.Duration) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "birdsense.json")
	store, err := db.OpenJSONFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	files, err := storage.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, path: path, files: files}
	f.keeper = New(store, files, Policy{Window: window}, slog.New(slog.DiscardHandler))
	f.keeper.now = func() time.Time { return testNow }
	return f
}

// card registers a received card and one audio file per status given, each
// with a recording and a clip in storage. A card's statuses are distinct, so
// a file's blob and clip are named after its status.
func (f *fixture) card(ref, status string, received time.Time, fileStatuses ...string) {
	f.t.Helper()
	ctx := f.t.Context()
	if _, err := f.store.CreateUpload(ctx, db.Upload{
		ID: ref, RecorderID: "SW-02", PulledOn: "2026-08-01",
		FileCount: len(fileStatuses), FilesUploaded: len(fileStatuses),
		Status: status, StartedAt: received, ReceivedAt: &received,
	}); err != nil {
		f.t.Fatal(err)
	}
	var docs []db.AudioFile
	for _, fs := range fileStatuses {
		doc := db.AudioFile{
			RecorderID: "SW-02", Path: "DATA/" + fs + ".WAV", SizeBytes: 100, Night: "2026-08-01",
			Status: fs, BlobName: storage.Name(ref + "/blob" + fs),
		}
		f.put(doc.BlobName, "audio")
		f.put(storage.ClipName(ref, "det_"+fs), "clip")
		docs = append(docs, doc)
	}
	if err := f.store.UpsertAudioFiles(ctx, ref, docs); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) put(name, body string) {
	f.t.Helper()
	if err := f.files.Put(f.t.Context(), name, strings.NewReader(body)); err != nil {
		f.t.Fatal(err)
	}
}

// has reports whether anything is stored under a name.
func (f *fixture) has(name string) bool {
	f.t.Helper()
	r, err := f.files.Open(f.t.Context(), name)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, r)
	r.Close()
	return true
}

func (f *fixture) cardFiles(ref string) []db.AudioFile {
	f.t.Helper()
	files, err := f.store.ListAudioFiles(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	return files
}

func (f *fixture) upload(ref string) db.Upload {
	f.t.Helper()
	u, err := f.store.GetUpload(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// TestSweepRemovesOriginals is the whole point: a card past its window loses
// its recordings and keeps its clips.
func TestSweepRemovesOriginals(t *testing.T) {
	f := newFixture(t, month)
	ref := "OWL-20260801-SR02"
	f.card(ref, db.StatusInReview, testNow.Add(-month-time.Hour), db.AudioAnalyzed, db.AudioFailed)
	f.keeper.Sweep(t.Context())

	for _, file := range f.cardFiles(ref) {
		if file.AudioDeletedAt == nil {
			t.Errorf("%s: AudioDeletedAt is unset", file.Path)
		} else if !file.AudioDeletedAt.Equal(testNow) {
			t.Errorf("%s: AudioDeletedAt = %v, want %v", file.Path, file.AudioDeletedAt, testNow)
		}
		if file.BlobName != "" {
			t.Errorf("%s: BlobName = %q, want it cleared", file.Path, file.BlobName)
		}
		if file.Status != db.AudioAnalyzed && file.Status != db.AudioFailed {
			t.Errorf("%s: status = %q, retention shouldn't change it", file.Path, file.Status)
		}
		if f.has(storage.Name(ref + "/blob" + file.Status)) {
			t.Errorf("%s: the recording is still stored", file.Path)
		}
		if !f.has(storage.ClipName(ref, "det_"+file.Status)) {
			t.Errorf("%s: its clip went with it", file.Path)
		}
	}
	if u := f.upload(ref); u.AudioDeletedAt == nil {
		t.Error("the card isn't marked as having lost its audio")
	}
	// The card reports nothing more to come once its audio has gone.
	if at := f.keeper.policy.ExpiresAt(f.upload(ref)); at != nil {
		t.Errorf("ExpiresAt after the sweep = %v, want nil", at)
	}
	// Sweeping again over a card with nothing left is quiet and harmless.
	f.keeper.Sweep(t.Context())

	// What the sweep wrote is in the database, not just in memory.
	reopened, err := db.OpenJSONFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.ListAudioFiles(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range stored {
		if file.AudioDeletedAt == nil || file.BlobName != "" {
			t.Errorf("%s after a reopen: AudioDeletedAt = %v, BlobName = %q", file.Path, file.AudioDeletedAt, file.BlobName)
		}
	}
	if u, err := reopened.GetUpload(t.Context(), ref); err != nil || u.AudioDeletedAt == nil {
		t.Errorf("the card after a reopen: AudioDeletedAt = %v, %v", u.AudioDeletedAt, err)
	}
}

// TestSweepKeepsWhatIsNotDue covers everything the sweep must not touch.
func TestSweepKeepsWhatIsNotDue(t *testing.T) {
	old, recent := testNow.Add(-month-time.Hour), testNow.Add(-time.Hour)
	cards := []struct {
		ref, status string
		received    time.Time
		file        string
		why         string
	}{
		{"OWL-20260901-SR02", db.StatusInReview, recent, db.AudioAnalyzed, "inside its window"},
		{"OWL-20260801-SR03", db.StatusProcessing, old, db.AudioUploaded, "still being analyzed"},
		{"OWL-20260801-SR04", db.StatusNeedsAttention, old, db.AudioUploaded, "never analyzed"},
		{"OWL-20260801-SR05", db.StatusInterrupted, old, db.AudioUploaded, "still being sent"},
	}
	f := newFixture(t, month)
	for _, c := range cards {
		f.card(c.ref, c.status, c.received, c.file)
	}
	f.keeper.Sweep(t.Context())

	for _, c := range cards {
		file := f.cardFiles(c.ref)[0]
		if file.BlobName == "" || file.AudioDeletedAt != nil {
			t.Errorf("%s (%s): its recording was removed", c.ref, c.why)
		}
		if !f.has(storage.Name(c.ref + "/blob" + c.file)) {
			t.Errorf("%s (%s): its recording is gone from storage", c.ref, c.why)
		}
		if f.upload(c.ref).AudioDeletedAt != nil {
			t.Errorf("%s (%s): the card is marked as having lost its audio", c.ref, c.why)
		}
	}
}

// TestSweepLeavesAPartCard covers a card BirdNET finished some of: the files
// it read expire, the ones still waiting keep their audio, and the card isn't
// marked done until they go too.
func TestSweepLeavesAPartCard(t *testing.T) {
	f := newFixture(t, month)
	ref := "OWL-20260801-SR06"
	f.card(ref, db.StatusNeedsAttention, testNow.Add(-month-time.Hour), db.AudioAnalyzed, db.AudioUploaded)
	f.keeper.Sweep(t.Context())

	for _, file := range f.cardFiles(ref) {
		gone := file.Status == db.AudioAnalyzed
		if (file.BlobName == "") != gone {
			t.Errorf("%s (%s): BlobName = %q", file.Path, file.Status, file.BlobName)
		}
		if f.has(storage.Name(ref+"/blob"+file.Status)) == gone {
			t.Errorf("%s (%s): the wrong thing is in storage", file.Path, file.Status)
		}
	}
	if u := f.upload(ref); u.AudioDeletedAt != nil {
		t.Error("the card is marked done while it still holds a recording")
	}
}

// TestPolicyOff is BIRDSENSE_AUDIO_RETENTION_DAYS=0: nothing expires, and a
// card offers no date.
func TestPolicyOff(t *testing.T) {
	f := newFixture(t, 0)
	ref := "OWL-20260801-SR07"
	f.card(ref, db.StatusResultsSent, testNow.Add(-10*365*24*time.Hour), db.AudioAnalyzed)
	f.keeper.Sweep(t.Context())

	if f.cardFiles(ref)[0].BlobName == "" {
		t.Error("a recording was removed with retention off")
	}
	if at := f.keeper.policy.ExpiresAt(f.upload(ref)); at != nil {
		t.Errorf("ExpiresAt with retention off = %v, want nil", at)
	}
	// Run returns at once rather than waking every few hours for nothing.
	done := make(chan struct{})
	go func() { defer close(done); f.keeper.Run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Run didn't return with retention off")
	}
}

// TestExpiresAt is the date the coordinator's card page shows.
func TestExpiresAt(t *testing.T) {
	received := testNow.Add(-10 * 24 * time.Hour)
	p := Policy{Window: month}
	u := db.Upload{Status: db.StatusInReview, ReceivedAt: &received}
	if at := p.ExpiresAt(u); at == nil || !at.Equal(received.Add(month)) {
		t.Errorf("ExpiresAt = %v, want %v", at, received.Add(month))
	}
	u.Status = db.StatusProcessing
	if at := p.ExpiresAt(u); at != nil {
		t.Errorf("a card still being analyzed reports %v, want nil", at)
	}
	if at := p.ExpiresAt(db.Upload{Status: db.StatusInReview}); at != nil {
		t.Errorf("a card that was never received reports %v, want nil", at)
	}
}

// failUpdates is a store whose UpdateAudioFile fails for one file, to stand
// in for a store that rejects the write after the blob has already gone.
type failUpdates struct {
	db.Store
	fileStatus string
}

func (s failUpdates) UpdateAudioFile(ctx context.Context, uploadID, id string, mutate func(*db.AudioFile) error) (db.AudioFile, error) {
	f, err := s.Store.GetAudioFile(ctx, uploadID, id)
	if err == nil && f.Status == s.fileStatus {
		return db.AudioFile{}, errors.New("the store said no")
	}
	return s.Store.UpdateAudioFile(ctx, uploadID, id, mutate)
}

// TestSweepKeepsACardWhoseMarkingFailed covers the blob going but the document
// not being marked. The card must stay unmarked, because a marked card reports
// no expiry date and is never swept again — which would strand that file's
// blobName on a blob that isn't there, for good.
func TestSweepKeepsACardWhoseMarkingFailed(t *testing.T) {
	f := newFixture(t, month)
	ref := "OWL-20260801-SR08"
	f.card(ref, db.StatusInReview, testNow.Add(-month-time.Hour), db.AudioAnalyzed, db.AudioFailed)
	f.keeper.store = failUpdates{Store: f.store, fileStatus: db.AudioFailed}
	f.keeper.Sweep(t.Context())

	if u := f.upload(ref); u.AudioDeletedAt != nil {
		t.Error("the card is marked done though one of its files wasn't marked")
	}
	if at := f.keeper.policy.ExpiresAt(f.upload(ref)); at == nil {
		t.Error("the card reports no expiry date, so no later sweep will come back to it")
	}

	// With the store working again, the next sweep finishes the job.
	f.keeper.store = f.store
	f.keeper.Sweep(t.Context())

	for _, file := range f.cardFiles(ref) {
		if file.BlobName != "" || file.AudioDeletedAt == nil {
			t.Errorf("%s: BlobName = %q, AudioDeletedAt = %v", file.Path, file.BlobName, file.AudioDeletedAt)
		}
	}
	if u := f.upload(ref); u.AudioDeletedAt == nil {
		t.Error("the card still isn't marked after a sweep that marked every file")
	}
}
