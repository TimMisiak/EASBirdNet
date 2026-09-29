package deletion

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

const ref = "OWL-20260914-SR03"

var now = time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC)

type fixture struct {
	store db.Store
	files storage.Store
	d     *Deleter
	at    time.Time // the deleter's clock
	clip  string
}

// newFixture is a card of two files, one detection and its clip, marked for
// deletion at now.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	files, err := storage.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{store: store, files: files, at: now, clip: storage.ClipName(ref, "det_owl")}
	f.d = New(store, files, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.d.now = func() time.Time { return f.at }

	ctx := t.Context()
	if _, err := store.CreateUpload(ctx, db.Upload{ID: ref, Status: db.StatusInReview}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAudioFiles(ctx, ref, []db.AudioFile{
		{Path: "DATA/a.wav", Status: db.AudioAnalyzed},
		{Path: "DATA/b.wav", Status: db.AudioAnalyzed},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDetections(ctx, ref, []db.Detection{{AudioFileID: db.AudioFileID(ref, "DATA/a.wav"), ScientificName: "Strix varia"}}); err != nil {
		t.Fatal(err)
	}
	if err := files.Put(ctx, f.clip, strings.NewReader("fLaC")); err != nil {
		t.Fatal(err)
	}
	if _, err := Mark(ctx, store, ref, now); err != nil {
		t.Fatal(err)
	}
	return f
}

// claim puts a step of the card's first file in analyzing, with a lease that
// runs out at until.
func (f *fixture) claim(t *testing.T, perch bool, until time.Time) {
	t.Helper()
	_, err := f.store.UpdateAudioFile(t.Context(), ref, db.AudioFileID(ref, "DATA/a.wav"), func(a *db.AudioFile) error {
		c := db.Claim{ClaimedBy: "worker:1", LeaseUntil: &until}
		if perch {
			a.Perch = &db.PerchRun{Status: db.PerchAnalyzing, Claim: c}
		} else {
			a.Status, a.Claim = db.AudioAnalyzing, c
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) gone(t *testing.T) bool {
	t.Helper()
	_, err := f.store.GetUpload(t.Context(), ref)
	return errors.Is(err, db.ErrNotFound)
}

func TestSweepDeletesAMarkedCard(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.at = now.Add(settle)
	if again := f.d.Sweep(ctx); again != 0 {
		t.Errorf("sweep left a card, again in %v", again)
	}
	if !f.gone(t) {
		t.Error("the card is still there")
	}
	files, _ := f.store.ListAudioFiles(ctx, ref)
	found, _ := f.store.ListDetections(ctx, db.DetectionFilter{UploadID: ref})
	if len(files) != 0 || len(found) != 0 {
		t.Errorf("left behind %d audio files and %d detections", len(files), len(found))
	}
	if _, err := f.files.Open(ctx, f.clip); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("clip after delete: err = %v, want ErrNotFound", err)
	}
}

func TestSweepLeavesACardThatIsNotMarked(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	const other = "OWL-20260915-SR04"
	if _, err := f.store.CreateUpload(ctx, db.Upload{ID: other, Status: db.StatusInReview}); err != nil {
		t.Fatal(err)
	}
	f.at = now.Add(settle)
	f.d.Sweep(ctx)
	if _, err := f.store.GetUpload(ctx, other); err != nil {
		t.Errorf("a card nobody deleted: %v", err)
	}
}

// A card is left for a minute after it is marked, so a claim the analysis
// queue was already making when it was marked has landed to be waited for.
func TestSweepWaitsForTheCardToSettle(t *testing.T) {
	f := newFixture(t)
	f.at = now.Add(20 * time.Second)
	if again := f.d.Sweep(t.Context()); again != settle-20*time.Second {
		t.Errorf("again in %v, want %v, when the card has settled", again, settle-20*time.Second)
	}
	if f.gone(t) {
		t.Error("the card went before it settled")
	}
}

func TestSweepWaitsForAnalysisToLetGo(t *testing.T) {
	for _, perch := range []bool{false, true} {
		name := map[bool]string{false: "BirdNET", true: "Perch"}[perch]
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()
			f.at = now.Add(settle)
			lease := f.at.Add(3 * time.Minute)
			f.claim(t, perch, lease)

			if again := f.d.Sweep(ctx); again != 3*time.Minute {
				t.Errorf("again in %v, want when the lease runs out", again)
			}
			if f.gone(t) {
				t.Fatal("the card went while analysis held one of its files")
			}
			if _, err := f.files.Open(ctx, f.clip); err != nil {
				t.Errorf("its clip went while analysis held one of its files: %v", err)
			}

			// A lease that has lapsed is a worker that died, and nobody's.
			f.at = lease
			if again := f.d.Sweep(ctx); again != 0 || !f.gone(t) {
				t.Errorf("after the lease: again in %v, card gone %v; want it gone", again, f.gone(t))
			}
		})
	}
}

// failsDelete fails DeleteUpload while left is above zero.
type failsDelete struct {
	db.Store
	left int
}

func (s *failsDelete) DeleteUpload(ctx context.Context, id string) error {
	if s.left > 0 {
		s.left--
		return errors.New("cosmos: 429 Too Many Requests")
	}
	return s.Store.DeleteUpload(ctx, id)
}

func TestAFailedSweepIsTriedAgainLater(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	failing := &failsDelete{Store: f.store, left: 2}
	f.d.store = failing
	f.at = now.Add(settle)

	if again := f.d.Sweep(ctx); again != firstRetry {
		t.Errorf("after one failure, again in %v, want %v", again, firstRetry)
	}
	u, err := f.store.GetUpload(ctx, ref)
	if err != nil || u.Status != db.StatusDeleting || u.StatusDetail != DetailRetrying {
		t.Fatalf("card after a failed pass = %+v, %v; want it deleting, saying it is retrying", u, err)
	}
	if again := f.d.Sweep(ctx); again != 2*firstRetry {
		t.Errorf("after two failures, again in %v, want %v", again, 2*firstRetry)
	}
	if again := f.d.Sweep(ctx); again != 0 || !f.gone(t) {
		t.Errorf("third pass: again in %v, card gone %v; want it gone", again, f.gone(t))
	}
	if f.d.failures != 0 {
		t.Errorf("failures = %d after a clean pass, want 0", f.d.failures)
	}
}

func TestRetryIsBounded(t *testing.T) {
	d := &Deleter{}
	var last time.Duration
	for range 20 {
		last = d.retry()
	}
	if last != maxRetry {
		t.Errorf("retry after 20 failures = %v, want %v", last, maxRetry)
	}
}

func TestMarkKeepsTheFirstTime(t *testing.T) {
	f := newFixture(t)
	u, err := Mark(t.Context(), f.store, ref, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !u.DeleteRequestedAt.Equal(now) {
		t.Errorf("marked again: deleteRequestedAt = %v, want %v kept", u.DeleteRequestedAt, now)
	}
	if _, err := Mark(t.Context(), f.store, "OWL-20260101-NOPE", now); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("marking a card that isn't there: err = %v, want ErrNotFound", err)
	}
}

func TestRunSweepsWhenWoken(t *testing.T) {
	f := newFixture(t)
	f.d.Settle = 0
	const other = "OWL-20260915-SR04"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.d.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, func() bool { return f.gone(t) })
	if _, err := f.store.CreateUpload(t.Context(), db.Upload{ID: other, Status: db.StatusInReview}); err != nil {
		t.Fatal(err)
	}
	if _, err := Mark(t.Context(), f.store, other, now); err != nil {
		t.Fatal(err)
	}
	f.d.Wake()
	waitFor(t, func() bool {
		_, err := f.store.GetUpload(t.Context(), other)
		return errors.Is(err, db.ErrNotFound)
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the deleter")
		}
	}
}
