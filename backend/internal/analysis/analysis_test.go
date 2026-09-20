package analysis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

var testNow = time.Date(2026, time.September, 14, 18, 0, 0, 0, time.UTC)

const ref = "OWL-20260914-SR02"

// fakeBirdNET answers for each file by the name it had on the card, which the
// queue passes as the temporary copy's extension and the audio's content.
type fakeBirdNET struct {
	mu    sync.Mutex
	calls []call
	// answer returns the result for one file's audio, or an error for the run.
	answer func(audio string) (birdnet.File, error)
	// cuts are the clips asked for, a slice per file; cutErr fails every cut.
	cuts   [][]birdnet.Clip
	cutErr error
}

type call struct {
	path, audio string
	opts        birdnet.Options
}

func (f *fakeBirdNET) Analyze(_ context.Context, paths []string, opts birdnet.Options) (birdnet.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := birdnet.Result{Model: "BirdNET_GLOBAL_6K_V2.4", Options: opts}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return birdnet.Result{}, err
		}
		f.calls = append(f.calls, call{path: p, audio: string(b), opts: opts})
		file, err := f.answer(string(b))
		if err != nil {
			return birdnet.Result{}, err
		}
		file.Path = p
		res.Files = append(res.Files, file)
	}
	return res, nil
}

// Cut writes each clip as its source's audio and its span, and reports every
// recording as an hour at 48 kHz.
func (f *fakeBirdNET) Cut(_ context.Context, source string, clips []birdnet.Clip) (birdnet.Recording, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cuts = append(f.cuts, clips)
	if f.cutErr != nil {
		return birdnet.Recording{}, f.cutErr
	}
	audio, err := os.ReadFile(source)
	if err != nil {
		return birdnet.Recording{}, err
	}
	rec := birdnet.Recording{DurationSec: 3600, SampleRate: 48000}
	for _, c := range clips {
		c.EndSec = min(c.EndSec, rec.DurationSec)
		if err := os.WriteFile(c.Path, fmt.Appendf(nil, "%s %g-%g", audio, c.StartSec, c.EndSec), 0o644); err != nil {
			return birdnet.Recording{}, err
		}
		rec.Clips = append(rec.Clips, c)
	}
	return rec, nil
}

type fixture struct {
	t     *testing.T
	store db.Store
	dir   string
	files storage.Store
	bird  *fakeBirdNET
	queue *Queue
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dir := t.TempDir()
	files, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: store, dir: dir, files: files, bird: &fakeBirdNET{}}
	f.queue = New(store, files, f.bird, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.queue.now = func() time.Time { return testNow }
	f.queue.retryDelay = time.Millisecond
	return f
}

// card stores a received card whose files are in storage, each holding its
// own path as its audio. A path with no audio isn't in storage.
func (f *fixture) card(status string, paths ...string) {
	f.t.Helper()
	ctx := f.t.Context()
	received := testNow.Add(-time.Hour)
	if _, err := f.store.CreateUpload(ctx, db.Upload{
		ID: ref, RecorderID: "SW-02",
		Recorder: db.RecorderSnapshot{Name: "Marymoor Park – Snag Row", Latitude: 47.66021, Longitude: -122.11384},
		PulledOn: "2026-09-14", FileCount: len(paths), FilesUploaded: len(paths),
		Status: status, StartedAt: received, ReceivedAt: &received,
	}); err != nil {
		f.t.Fatal(err)
	}
	var docs []db.AudioFile
	for _, p := range paths {
		doc := db.AudioFile{RecorderID: "SW-02", Path: p, SizeBytes: 100, Night: "2026-09-12", Status: db.AudioUploaded}
		if !strings.Contains(p, "missing") {
			doc.BlobName = storage.Name(ref + "/" + strings.ReplaceAll(path.Base(p), ".", "_"))
			local := filepath.Join(f.dir, filepath.FromSlash(doc.BlobName))
			if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
				f.t.Fatal(err)
			}
			if err := os.WriteFile(local, []byte(p), 0o644); err != nil {
				f.t.Fatal(err)
			}
		}
		docs = append(docs, doc)
	}
	if err := f.store.UpsertAudioFiles(ctx, ref, docs); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) file(p string) db.AudioFile {
	f.t.Helper()
	file, err := f.store.GetAudioFile(f.t.Context(), ref, db.AudioFileID(ref, p))
	if err != nil {
		f.t.Fatal(err)
	}
	return file
}

// stored is what file storage holds under a name.
func (f *fixture) stored(name string) string {
	f.t.Helper()
	r, err := f.files.Open(f.t.Context(), name)
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func (f *fixture) upload() db.Upload {
	f.t.Helper()
	u, err := f.store.GetUpload(f.t.Context(), ref)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

const (
	owlFile    = "DATA/20260912/Marymoor_20260912_230000(-0700).WAV"
	quietFile  = "DATA/20260912/Marymoor_20260913_000000.wav"
	brokenFile = "DATA/20260912/broken.flac"
)

func owls(audio string) (birdnet.File, error) {
	switch audio {
	case owlFile:
		return birdnet.File{Detections: []birdnet.Detection{
			{StartSec: 732, EndSec: 735, ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.91},
			{StartSec: 732, EndSec: 735, ScientificName: "Bubo virginianus", CommonName: "Great Horned Owl", Confidence: 0.3},
		}}, nil
	case brokenFile:
		return birdnet.File{Error: "unreadable audio"}, nil
	}
	return birdnet.File{}, nil
}

func TestACardIsAnalyzedFileByFile(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, quietFile, brokenFile)

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}

	// One run per file, each on a copy that keeps its extension, located at
	// the recorder in the week of the night.
	if len(f.bird.calls) != 3 {
		t.Fatalf("BirdNET ran %d times, want once per file", len(f.bird.calls))
	}
	for _, c := range f.bird.calls {
		if filepath.Ext(c.path) != strings.ToLower(path.Ext(c.audio)) {
			t.Errorf("%s was analyzed as %s; the extension picks the decoder", c.audio, c.path)
		}
		if l := c.opts.Location; l == nil || l.Latitude != 47.66021 || l.Week != 34 {
			t.Errorf("%s location = %+v, want the recorder in week 34", c.audio, l)
		}
		if _, err := os.Stat(c.path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the copy of %s was left behind", c.audio)
		}
	}

	owl := f.file(owlFile)
	start := time.Date(2026, time.September, 13, 6, 0, 0, 0, time.UTC) // 23:00 at -0700
	if owl.Status != db.AudioAnalyzed || owl.DetectionCount != 2 || owl.AnalyzedAt == nil || owl.RecordedAt == nil || !owl.RecordedAt.Equal(start) {
		t.Errorf("owl file = %+v; want analyzed with 2 detections, recorded at %v", owl, start)
	}
	dets, err := f.store.ListDetections(t.Context(), db.DetectionFilter{UploadID: ref, AudioFileID: owl.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 2 {
		t.Fatalf("got %d detections for the owl file, want 2", len(dets))
	}
	d := dets[0]
	if want := start.Add(732 * time.Second); !d.DetectedAt.Equal(want) || d.Night != "2026-09-12" || d.RecorderID != "SW-02" ||
		d.ReviewStatus != db.ReviewUnreviewed || d.EndSec != 735 || d.Confidence == 0 {
		t.Errorf("detection = %+v; want heard at %v, unreviewed, on the file's night", d, want)
	}
	// Each detection's clip is cut around it and stored under its id.
	for _, d := range dets {
		if c := d.Clip; c == nil || c.StartSec != 731 || c.EndSec != 736 || c.BlobName != storage.ClipName(ref, d.ID) {
			t.Errorf("%s clip = %+v; want 731-736 s, named by the detection", d.CommonName, c)
		} else if got := f.stored(c.BlobName); got != owlFile+" 731-736" {
			t.Errorf("%s stored clip = %q", d.CommonName, got)
		}
	}
	if owl.DurationSec != 3600 || owl.SampleRate != 48000 {
		t.Errorf("owl file is %v s at %d Hz; want what the cut read", owl.DurationSec, owl.SampleRate)
	}

	if quiet := f.file(quietFile); quiet.Status != db.AudioAnalyzed || quiet.DetectionCount != 0 ||
		!quiet.RecordedAt.Equal(time.Date(2026, time.September, 13, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("quiet file = %+v; want analyzed, nothing heard, recorded at midnight Pacific", quiet)
	}
	if broken := f.file(brokenFile); broken.Status != db.AudioFailed || broken.StatusDetail != "unreadable audio" {
		t.Errorf("broken file = %+v; want failed as unreadable", broken)
	}

	u := f.upload()
	if u.Status != db.StatusNeedsAttention || u.StatusDetail != "1 file not analyzed" ||
		u.FilesAnalyzed != 2 || u.FilesFailed != 1 || u.DetectionCount != 2 || u.ProcessedAt == nil {
		t.Errorf("card = %s (%s), %d analyzed, %d failed, %d detections; want needs_attention with the tally",
			u.Status, u.StatusDetail, u.FilesAnalyzed, u.FilesFailed, u.DetectionCount)
	}
	if a := u.Analysis; a == nil || a.Model != "BirdNET_GLOBAL_6K_V2.4" || a.MinConfidence != birdnet.DefaultMinConfidence ||
		a.FinishedAt == nil || !a.FinishedAt.Equal(testNow) {
		t.Errorf("analysis = %+v", a)
	}

	// Nothing is left, so another pass runs nothing.
	if err := f.queue.drain(t.Context()); err != nil || len(f.bird.calls) != 3 {
		t.Errorf("second pass: %v, %d runs", err, len(f.bird.calls))
	}
}

func TestConsecutiveWindowsAreOneDetection(t *testing.T) {
	f := newFixture(t)
	window := func(start float64, scientific, common string, confidence float64) birdnet.Detection {
		return birdnet.Detection{StartSec: start, EndSec: start + 3, ScientificName: scientific, CommonName: common, Confidence: confidence}
	}
	f.bird.answer = func(string) (birdnet.File, error) {
		return birdnet.File{Detections: []birdnet.Detection{
			window(0, "Strix varia", "Barred Owl", 0.4),
			window(3, "Strix varia", "Barred Owl", 0.8),
			window(3, "Bubo virginianus", "Great Horned Owl", 0.3),
			window(6, "Strix varia", "Barred Owl", 0.5),
			// A window without it in between: heard again, not still.
			window(12, "Strix varia", "Barred Owl", 0.3),
		}}, nil
	}
	f.card(db.StatusProcessing, quietFile)
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}

	file := f.file(quietFile)
	dets, err := f.store.ListDetections(t.Context(), db.DetectionFilter{UploadID: ref, AudioFileID: file.ID})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range dets {
		got = append(got, fmt.Sprintf("%s %g-%g at %g, clip %g-%g", d.CommonName, d.StartSec, d.EndSec, d.Confidence, d.Clip.StartSec, d.Clip.EndSec))
	}
	want := []string{
		"Barred Owl 0-9 at 0.8, clip 0-10",
		"Great Horned Owl 3-6 at 0.3, clip 2-7",
		"Barred Owl 12-15 at 0.3, clip 11-16",
	}
	if !slices.Equal(got, want) {
		t.Errorf("detections:\n got  %q\n want %q", got, want)
	}
	if dets[0].ID != db.DetectionID(file.ID, 0, "Strix varia") {
		t.Errorf("merged id = %s; want it derived from the run's start, so a re-run overwrites it", dets[0].ID)
	}
	if file.DetectionCount != 3 || f.upload().DetectionCount != 3 {
		t.Errorf("file counts %d, card %d; want the 3 merged detections", file.DetectionCount, f.upload().DetectionCount)
	}
}

func TestClipSpan(t *testing.T) {
	run := func(start, end, peak float64) heard {
		return heard{Detection: birdnet.Detection{StartSec: start, EndSec: end}, PeakStartSec: peak, PeakEndSec: peak + 3}
	}
	cases := []struct {
		name       string
		run        heard
		start, end float64
	}{
		{"one window, a second either side", run(732, 735, 732), 731, 736},
		{"at the start of the file", run(0, 3, 0), 0, 4},
		{"a long run is cut around its peak", run(0, 300, 150), 136.5, 166.5},
		{"a peak near the start of a long run", run(60, 300, 60), 59, 89},
		{"a peak at the end of a long run", run(0, 300, 297), 271, 301},
	}
	for _, c := range cases {
		if start, end := clipSpan(c.run); start != c.start || end != c.end {
			t.Errorf("%s: clip %g-%g, want %g-%g", c.name, start, end, c.start, c.end)
		}
	}
}

func TestCuttingFailingIsRetriedThenTheFileFails(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.bird.cutErr = errors.New("birdnet: clip.py: exit status 1\nLibsndfileError")
	f.card(db.StatusProcessing, owlFile)
	for attempt := 1; attempt < maxAttempts; attempt++ {
		if err := f.queue.drain(t.Context()); !errors.Is(err, errRetry) {
			t.Fatalf("attempt %d: err = %v, want a pause to retry", attempt, err)
		}
	}
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	owl := f.file(owlFile)
	if owl.Status != db.AudioFailed || !strings.Contains(owl.StatusDetail, "cutting clips") {
		t.Errorf("file = %s (%q); want failed while cutting clips", owl.Status, owl.StatusDetail)
	}
	if dets, _ := f.store.ListDetections(t.Context(), db.DetectionFilter{UploadID: ref}); len(dets) != 0 {
		t.Errorf("stored %d detections with no clips", len(dets))
	}
}

func TestACleanCardGoesToReview(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, quietFile)
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := f.upload(); u.Status != db.StatusInReview || u.StatusDetail != "" || u.FilesAnalyzed != 2 {
		t.Errorf("card = %s (%q), %d analyzed; want in_review", u.Status, u.StatusDetail, u.FilesAnalyzed)
	}
}

// A file that was mid-analysis when the server stopped is run again, and a
// card still being sent isn't touched.
func TestRunPicksUpWhereARestartLeftOff(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, quietFile)
	if _, err := f.store.UpdateAudioFile(t.Context(), ref, db.AudioFileID(ref, owlFile), func(a *db.AudioFile) error {
		a.Status = db.AudioAnalyzing
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sending := "OWL-20260914-SR03"
	if _, err := f.store.CreateUpload(t.Context(), db.Upload{ID: sending, Status: db.StatusInProgress}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertAudioFiles(t.Context(), sending, []db.AudioFile{{Path: "a.wav", Status: db.AudioUploaded, BlobName: "uploads/x"}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.queue.Run(ctx)
	}()
	waitFor(t, func() bool { return f.upload().Status == db.StatusInReview })

	// A card that arrives later is picked up when the API says so.
	later := "OWL-20260914-SR05"
	received := testNow
	if _, err := f.store.CreateUpload(t.Context(), db.Upload{ID: later, Status: db.StatusProcessing, ReceivedAt: &received}); err != nil {
		t.Fatal(err)
	}
	blob := storage.Name(later + "/q")
	os.MkdirAll(filepath.Join(f.dir, "uploads", later), 0o755)
	os.WriteFile(filepath.Join(f.dir, filepath.FromSlash(blob)), []byte(quietFile), 0o644)
	if err := f.store.UpsertAudioFiles(t.Context(), later, []db.AudioFile{{Path: "q.wav", Night: "2026-09-13", Status: db.AudioUploaded, BlobName: blob}}); err != nil {
		t.Fatal(err)
	}
	f.queue.Enqueue(later)
	waitFor(t, func() bool {
		u, err := f.store.GetUpload(t.Context(), later)
		return err == nil && u.Status == db.StatusInReview
	})
	cancel()
	<-done

	if n := len(f.bird.calls); n != 3 {
		t.Errorf("BirdNET ran %d times, want 3 (two files, then the later card's one)", n)
	}
	if u, _ := f.store.GetUpload(t.Context(), sending); u.Status != db.StatusInProgress || u.Analysis != nil {
		t.Errorf("a card still being sent was analyzed: %+v", u)
	}
}

func TestBirdNETFailingIsRetriedThenTheFileFails(t *testing.T) {
	f := newFixture(t)
	runs := 0
	f.bird.answer = func(audio string) (birdnet.File, error) {
		runs++
		return birdnet.File{}, errors.New("birdnet: analyze.py: signal: killed\nMemoryError")
	}
	f.card(db.StatusProcessing, quietFile)

	for attempt := 1; attempt < maxAttempts; attempt++ {
		if err := f.queue.drain(t.Context()); !errors.Is(err, errRetry) {
			t.Fatalf("attempt %d: err = %v, want a pause to retry", attempt, err)
		}
		if file := f.file(quietFile); file.Status != db.AudioUploaded {
			t.Fatalf("attempt %d: file = %s, want it queued again", attempt, file.Status)
		}
	}
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatalf("last attempt: %v", err)
	}
	file := f.file(quietFile)
	if runs != maxAttempts || file.Status != db.AudioFailed || !strings.Contains(file.StatusDetail, "signal: killed") ||
		strings.Contains(file.StatusDetail, "MemoryError") {
		t.Errorf("after %d runs, file = %s (%q); want failed with the first line of the error", runs, file.Status, file.StatusDetail)
	}
	if u := f.upload(); u.Status != db.StatusNeedsAttention || u.FilesFailed != 1 {
		t.Errorf("card = %s, %d failed", u.Status, u.FilesFailed)
	}
}

func TestAudioMissingFromStorageFails(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, "DATA/missing.wav")
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if file := f.file("DATA/missing.wav"); file.Status != db.AudioFailed || file.StatusDetail != "the audio isn't in storage" {
		t.Errorf("file = %s (%q)", file.Status, file.StatusDetail)
	}
	if len(f.bird.calls) != 0 {
		t.Errorf("BirdNET ran without the audio")
	}
}

func TestACardWithNoFilesOnRecordIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.card(db.StatusProcessing)
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u := f.upload(); u.Status != db.StatusProcessing || u.ProcessedAt != nil {
		t.Errorf("card = %s; want it still processing", u.Status)
	}
}

// deletedMidWrite deletes the card just before the queue stores its detections,
// after the queue has checked the card is still there.
type deletedMidWrite struct{ db.Store }

func (s deletedMidWrite) UpsertDetections(ctx context.Context, uploadID string, ds []db.Detection) error {
	if err := s.Store.DeleteUpload(ctx, uploadID); err != nil {
		return err
	}
	return s.Store.UpsertDetections(ctx, uploadID, ds)
}

func TestACardDeletedDuringAnalysisLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		// clipsGone is whether the card's clips are kept out of storage too. A
		// delete between the queue's check and its writes can leave them.
		clipsGone bool
	}{
		{"while BirdNET runs", func(f *fixture) {
			f.bird.answer = func(audio string) (birdnet.File, error) {
				if err := f.store.DeleteUpload(context.Background(), ref); err != nil {
					return birdnet.File{}, err
				}
				return owls(audio)
			}
		}, true},
		{"while its detections are stored", func(f *fixture) {
			f.bird.answer = owls
			f.queue.store = deletedMidWrite{f.store}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.card(db.StatusProcessing, owlFile, quietFile)
			tc.setup(f)
			if err := f.queue.drain(t.Context()); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if n := len(f.bird.calls); n != 1 {
				t.Errorf("BirdNET ran %d times, want once, for the file it was on", n)
			}
			ctx := t.Context()
			if _, err := f.store.GetUpload(ctx, ref); !errors.Is(err, db.ErrNotFound) {
				t.Errorf("card: err = %v, want ErrNotFound", err)
			}
			files, _ := f.store.ListAudioFiles(ctx, ref)
			found, _ := f.store.ListDetections(ctx, db.DetectionFilter{UploadID: ref})
			if len(files) != 0 || len(found) != 0 {
				t.Errorf("left behind %d audio files and %d detections", len(files), len(found))
			}
			if clips, _ := filepath.Glob(filepath.Join(f.dir, "clips", ref, "*")); tc.clipsGone && len(clips) != 0 {
				t.Errorf("left %d clips in storage", len(clips))
			}
		})
	}
}

func TestRecordedAt(t *testing.T) {
	cases := map[string]string{
		"DATA/Marymoor_20260723_160624(-0700).wav": "2026-07-23T23:06:24Z",
		"DATA/20260725/20260725_040000.WAV":        "2026-07-25T11:00:00Z", // Pacific daylight time
		"SMM01234_20260115_220000.wav":             "2026-01-16T06:00:00Z", // Pacific standard time
		"20260115_220000_+0100.wav":                "2026-01-16T06:00:00Z", // an offset only counts in parentheses
		"a_20260231_010000.wav":                    "",
		"recording.wav":                            "",
		"x120260115_220000.wav":                    "",
		// A recorder that isn't on Pacific time. Each of these is a different
		// time from the Pacific reading of the same clock, so a lost offset
		// shows up here rather than being masked by the two matching.
		"Marymoor_20260723_160624(+0000).wav": "2026-07-23T16:06:24Z",
		"Kirkland_20260723_160624(-0400).wav": "2026-07-23T20:06:24Z",
		"20260115_220000(+0100).wav":          "2026-01-15T21:00:00Z",
		"20260115_220000 (+0100).wav":         "2026-01-15T21:00:00Z",
		"20260115_220000_001(+0100).wav":      "2026-01-15T21:00:00Z",
	}
	for p, want := range cases {
		got, ok := RecordedAt(p)
		switch {
		case want == "" && ok:
			t.Errorf("RecordedAt(%q) = %v, want none", p, got)
		case want != "" && (!ok || got.Format(time.RFC3339) != want):
			t.Errorf("RecordedAt(%q) = %v, %v; want %s", p, got, ok, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the queue")
		}
	}
}

// rejects is a store that fails one write, to stand in for a store that goes
// on rejecting it — a rule that won't pass, or an identity that lost its role.
type rejects struct {
	db.Store
	detections bool
	audioFile  bool
}

func (s rejects) UpsertDetections(ctx context.Context, uploadID string, ds []db.Detection) error {
	if s.detections {
		return errors.New("the store said no")
	}
	return s.Store.UpsertDetections(ctx, uploadID, ds)
}

func (s rejects) UpdateAudioFile(ctx context.Context, uploadID, id string, mutate func(*db.AudioFile) error) (db.AudioFile, error) {
	// Only the write that records the result. The queue's other writes to a
	// file — putting it back in the queue, failing it — have to go through, or
	// the test is of a store that rejects everything rather than one write.
	if s.audioFile {
		f, err := s.Store.GetAudioFile(ctx, uploadID, id)
		if err == nil && mutate(&f) == nil && f.Status == db.AudioAnalyzed {
			return db.AudioFile{}, errors.New("the store said no")
		}
	}
	return s.Store.UpdateAudioFile(ctx, uploadID, id, mutate)
}

// refusesPut is file storage that won't take a clip.
type refusesPut struct{ storage.Store }

func (s refusesPut) Put(context.Context, string, io.ReadSeeker) error {
	return errors.New("PutBlob: 403 AuthorizationPermissionMismatch")
}

// TestStoringFailingIsRetriedThenTheFileFails covers the steps after BirdNET
// has read the file. A failure there used to bypass the attempt counter, so
// the queue re-ran the whole analysis for good and never moved on.
func TestStoringFailingIsRetriedThenTheFileFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
		want  string
	}{
		{"storing a clip", func(f *fixture) { f.queue.files = refusesPut{f.files} }, "storing a clip"},
		{"storing the detections", func(f *fixture) { f.queue.store = rejects{Store: f.store, detections: true} }, "storing the detections"},
		{"marking the file analyzed", func(f *fixture) { f.queue.store = rejects{Store: f.store, audioFile: true} }, "marking the file analyzed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.bird.answer = owls
			f.card(db.StatusProcessing, owlFile)
			tc.setup(f)

			for attempt := 1; attempt < maxAttempts; attempt++ {
				if err := f.queue.drain(t.Context()); !errors.Is(err, errRetry) {
					t.Fatalf("attempt %d: err = %v, want a pause to retry", attempt, err)
				}
				if file := f.file(owlFile); file.Status != db.AudioUploaded {
					t.Fatalf("attempt %d: file = %s, want it queued again", attempt, file.Status)
				}
			}
			// The last attempt gives up on the file instead of trying forever.
			if err := f.queue.drain(t.Context()); err != nil {
				t.Fatalf("last attempt: %v", err)
			}
			if n := len(f.bird.calls); n != maxAttempts {
				t.Errorf("BirdNET ran %d times, want %d", n, maxAttempts)
			}
			file := f.file(owlFile)
			if file.Status != db.AudioFailed || !strings.Contains(file.StatusDetail, tc.want) {
				t.Errorf("file = %s (%q); want failed while %s", file.Status, file.StatusDetail, tc.want)
			}
			// The card moves on, so nothing queued behind it waits on this file.
			if u := f.upload(); u.Status != db.StatusNeedsAttention || u.FilesFailed != 1 {
				t.Errorf("card = %s, %d failed; want needs_attention", u.Status, u.FilesFailed)
			}
		})
	}
}
