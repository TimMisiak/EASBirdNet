package analysis

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// perchOwls is Perch hearing the owl file in 5-second windows: the Barred Owl
// BirdNET heard, at the same moment, and a Western Screech-Owl it didn't.
func perchOwls(audio string) (birdnet.File, error) {
	if audio != owlFile {
		return birdnet.File{}, nil
	}
	return birdnet.File{Detections: []birdnet.Detection{
		{StartSec: 730, EndSec: 735, ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.6},
		{StartSec: 735, EndSec: 740, ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.8},
		{StartSec: 900, EndSec: 905, ScientificName: "Megascops kennicottii", CommonName: "Western Screech-Owl", Confidence: 0.4},
	}}, nil
}

// models is which model each run was, in the order they ran, with the file.
func (f *fixture) runs() []string {
	var out []string
	for _, c := range f.bird.calls {
		out = append(out, birdnetOr(c.opts.Model)+" "+c.audio)
	}
	return out
}

func birdnetOr(model string) string {
	if model == "" {
		return birdnet.ModelBirdNET
	}
	return model
}

func TestPerchRunsAfterBirdNETAsAStepOfItsOwn(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.bird.perchAnswer = perchOwls
	f.card(db.StatusProcessing, owlFile, quietFile)

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}

	// BirdNET over the whole card first, then Perch over each file, with the
	// same location and threshold.
	want := []string{"birdnet " + owlFile, "birdnet " + quietFile, "perch " + owlFile, "perch " + quietFile}
	if got := f.runs(); !slices.Equal(got, want) {
		t.Errorf("runs:\n got  %q\n want %q", got, want)
	}
	for _, c := range f.bird.calls {
		if l := c.opts.Location; l == nil || l.Latitude != 47.66021 || l.Week != 34 || c.opts.MinConfidence != birdnet.DefaultMinConfidence {
			t.Errorf("%s %s ran with %+v, want the recorder's location and the card's threshold", c.opts.Model, c.audio, c.opts)
		}
	}

	owl := f.file(owlFile)
	if owl.Status != db.AudioAnalyzed || owl.DetectionCount != 2 {
		t.Errorf("owl file = %s with %d detections; BirdNET's result should be its own", owl.Status, owl.DetectionCount)
	}
	if p := owl.Perch; p == nil || p.Status != db.PerchAnalyzed || p.DetectionCount != 2 || p.AnalyzedAt == nil {
		t.Errorf("owl file's Perch step = %+v, want analyzed with 2 detections", p)
	}

	// Both models' detections are stored, each saying which model heard it.
	// The Barred Owl heard by both at 732 s and 730 s is two detections.
	dets, err := f.store.ListDetections(t.Context(), db.DetectionFilter{UploadID: ref, AudioFileID: owl.ID, Model: db.ModelPerch})
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 2 {
		t.Fatalf("got %d Perch detections, want 2 (the owl's windows merged, and the screech-owl)", len(dets))
	}
	barred := dets[0]
	if barred.Model != db.ModelPerch || barred.StartSec != 730 || barred.EndSec != 740 || barred.Confidence != 0.8 ||
		barred.ID != db.ModelDetectionID(db.ModelPerch, owl.ID, 730_000, "Strix varia") {
		t.Errorf("Perch's Barred Owl = %+v; want 730-740 s at 0.8, marked perch", barred)
	}
	if c := barred.Clip; c == nil || c.StartSec != 729 || c.EndSec != 741 || f.stored(c.BlobName) != owlFile+" 729-741" {
		t.Errorf("Perch's Barred Owl clip = %+v", c)
	}
	birdnetDets, err := f.store.ListDetections(t.Context(), db.DetectionFilter{UploadID: ref, AudioFileID: owl.ID, Model: db.ModelBirdNET})
	if err != nil {
		t.Fatal(err)
	}
	if len(birdnetDets) != 2 || birdnetDets[0].Model != db.ModelBirdNET {
		t.Errorf("BirdNET's detections = %+v, want its two, marked birdnet", birdnetDets)
	}

	u := f.upload()
	if u.Status != db.StatusInReview || u.DetectionCount != 2 || u.PerchDetectionCount != 2 {
		t.Errorf("card = %s, %d BirdNET and %d Perch detections; want in_review with each model's own count", u.Status, u.DetectionCount, u.PerchDetectionCount)
	}
	if a := u.Analysis; a == nil || a.Model != "BirdNET_GLOBAL_6K_V2.4" || a.PerchModel != "Perch_v2" {
		t.Errorf("analysis = %+v, want both models named", a)
	}

	// Nothing is left for either model.
	if err := f.queue.drain(t.Context()); err != nil || len(f.bird.calls) != 4 {
		t.Errorf("second pass: %v, %d runs", err, len(f.bird.calls))
	}
}

// A card still waiting for Perch is still processing: it isn't in review yet,
// and the retention sweep, which only takes a finished card's audio, leaves
// the recording Perch has yet to read.
func TestACardWaitsInProcessingForPerch(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile)

	if err := f.queue.processCard(t.Context(), f.upload()); err != nil {
		t.Fatal(err)
	}
	if u := f.upload(); u.Status != db.StatusProcessing || u.FilesAnalyzed != 1 {
		t.Errorf("after BirdNET alone, card = %s with %d analyzed; want processing", u.Status, u.FilesAnalyzed)
	}
	if p := f.file(owlFile).Perch; p == nil || p.Status != db.PerchQueued {
		t.Errorf("owl file's Perch step = %+v, want queued", p)
	}
}

// A new card gets BirdNET as soon as it arrives, rather than waiting for Perch
// to get through a card that came before it.
func TestANewCardDoesNotWaitBehindPerch(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	later := "OWL-20260914-SR05"
	arrived := false
	f.bird.perchAnswer = func(audio string) (birdnet.File, error) {
		if !arrived {
			arrived = true
			received := testNow
			if _, err := f.store.CreateUpload(context.Background(), db.Upload{ID: later, Status: db.StatusProcessing, ReceivedAt: &received}); err != nil {
				return birdnet.File{}, err
			}
			blob := storage.Name(later + "/q")
			os.MkdirAll(filepath.Join(f.dir, "uploads", later), 0o755)
			os.WriteFile(filepath.Join(f.dir, filepath.FromSlash(blob)), []byte("later.wav"), 0o644)
			if err := f.store.UpsertAudioFiles(context.Background(), later, []db.AudioFile{{Path: "later.wav", Night: "2026-09-13", Status: db.AudioUploaded, BlobName: blob}}); err != nil {
				return birdnet.File{}, err
			}
		}
		return birdnet.File{}, nil
	}
	f.card(db.StatusProcessing, owlFile, quietFile)

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"birdnet " + owlFile, "birdnet " + quietFile,
		"perch " + owlFile,
		// The later card arrived during that run, and goes next.
		"birdnet later.wav",
		"perch " + quietFile, "perch later.wav",
	}
	if got := f.runs(); !slices.Equal(got, want) {
		t.Errorf("runs:\n got  %q\n want %q", got, want)
	}
}

// Perch failing on a file is Perch's failure alone: BirdNET's result on the
// file stands, and the card goes to review rather than needing attention.
func TestPerchFailingIsRetriedThenLeavesBirdNETsResult(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.bird.perchAnswer = func(string) (birdnet.File, error) {
		return birdnet.File{}, errors.New("birdnet: analyze.py: signal: killed\nMemoryError")
	}
	f.card(db.StatusProcessing, owlFile)

	for attempt := 1; attempt < maxAttempts; attempt++ {
		if err := f.queue.drain(t.Context()); !errors.Is(err, errRetry) || !strings.Contains(err.Error(), "perch") {
			t.Fatalf("attempt %d: err = %v, want a pause to retry Perch", attempt, err)
		}
		if p := f.file(owlFile).Perch; p == nil || p.Status != db.PerchQueued {
			t.Fatalf("attempt %d: Perch step = %+v, want it queued again", attempt, p)
		}
	}
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatalf("last attempt: %v", err)
	}
	// BirdNET ran once; only Perch was retried.
	if got := f.runs(); len(got) != 1+maxAttempts || got[0] != "birdnet "+owlFile {
		t.Errorf("runs = %q, want BirdNET once and Perch %d times", got, maxAttempts)
	}
	owl := f.file(owlFile)
	if p := owl.Perch; p == nil || p.Status != db.PerchFailed || !strings.Contains(p.StatusDetail, "signal: killed") {
		t.Errorf("Perch step = %+v, want failed with the error's first line", p)
	}
	if owl.Status != db.AudioAnalyzed || owl.DetectionCount != 2 {
		t.Errorf("owl file = %s with %d detections; Perch failing shouldn't touch BirdNET's result", owl.Status, owl.DetectionCount)
	}
	if u := f.upload(); u.Status != db.StatusInReview || u.FilesFailed != 0 || u.DetectionCount != 2 {
		t.Errorf("card = %s (%s), %d failed; want in_review", u.Status, u.StatusDetail, u.FilesFailed)
	}
}

func TestPerchOnAFileItCantReadFailsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.bird.perchAnswer = func(string) (birdnet.File, error) { return birdnet.File{Error: "unreadable audio"}, nil }
	f.card(db.StatusProcessing, owlFile)

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := f.file(owlFile).Perch; p == nil || p.Status != db.PerchFailed || p.StatusDetail != "unreadable audio" {
		t.Errorf("Perch step = %+v, want failed as unreadable", p)
	}
	if u := f.upload(); u.Status != db.StatusInReview {
		t.Errorf("card = %s, want in_review", u.Status)
	}
}

// Turning Perch off lets a card that was waiting for it finish, and its files
// say Perch never ran rather than that it is still coming.
func TestTurningPerchOffLetsAWaitingCardFinish(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile)
	if err := f.queue.processCard(t.Context(), f.upload()); err != nil {
		t.Fatal(err)
	}

	f.queue.Perch = false
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := f.file(owlFile).Perch; p != nil {
		t.Errorf("Perch step = %+v, want none", p)
	}
	if u := f.upload(); u.Status != db.StatusInReview {
		t.Errorf("card = %s, want in_review", u.Status)
	}
	if got := f.runs(); len(got) != 1 {
		t.Errorf("runs = %q, want BirdNET's alone", got)
	}
}

// With Perch off, nothing about BirdNET's analysis mentions it.
func TestPerchOffQueuesNothingForIt(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile)
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.file(owlFile).Perch != nil || f.upload().Analysis.PerchModel != "" || f.bird.perchChecks != 0 {
		t.Errorf("Perch was involved with it off")
	}
}

// Perch turned on but not installed holds the queue and says why, the same
// as BirdNET missing: it was asked for, so finishing cards without it would
// be the wrong answer, quietly.
func TestPerchOnButUnavailableSaysWhy(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.bird.answer = owls
	f.bird.perchErr = errors.New("birdnet: can't run Perch (install analyzer/requirements-perch.txt)")
	f.card(db.StatusProcessing, owlFile)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.queue.Run(ctx)
	}()
	waitFor(t, func() bool { return f.queue.Status().State == StateUnavailable })
	cancel()
	<-done
	if s := f.queue.Status(); !strings.Contains(s.Detail, "requirements-perch.txt") {
		t.Errorf("status = %+v, want Perch's check error", s)
	}
	if u := f.upload(); u.Analysis != nil {
		t.Errorf("card was analyzed while Perch couldn't run: %+v", u)
	}
}
