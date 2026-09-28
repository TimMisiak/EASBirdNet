package analysis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// claim puts a file in BirdNET's hands as someone else's, on a lease until
// the given time.
func (f *fixture) claim(p, worker string, until time.Time) {
	f.t.Helper()
	if _, err := f.store.UpdateAudioFile(f.t.Context(), ref, db.AudioFileID(ref, p), func(a *db.AudioFile) error {
		a.Status, a.ClaimedBy, a.LeaseUntil = db.AudioAnalyzing, worker, &until
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// Two workers over the same card never run a file twice: each file is
// claimed by exactly one of them.
func TestTwoWorkersShareACard(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("DATA/20260912/Marymoor_20260912_%02d0000.wav", i))
	}
	f.card(db.StatusProcessing, paths...)
	other := New(f.store, f.files, f.bird, f.queue.log)
	other.now, other.retryDelay = f.queue.now, f.queue.retryDelay
	f.queue.worker, other.worker = "worker-a", "worker-b"

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, q := range []*Queue{f.queue, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- q.drain(t.Context())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	ran := map[string]int{}
	for _, c := range f.bird.calls {
		ran[c.audio]++
	}
	for _, p := range paths {
		if ran[p] != 1 {
			t.Errorf("%s ran %d times, want once", p, ran[p])
		}
		if file := f.file(p); file.Status != db.AudioAnalyzed || file.ClaimedBy != "" || file.LeaseUntil != nil {
			t.Errorf("%s = %s, claimed by %q; want analyzed and let go", p, file.Status, file.ClaimedBy)
		}
	}
	if u := f.upload(); u.Status != db.StatusInReview || u.FilesAnalyzed != len(paths) {
		t.Errorf("card = %s, %d analyzed", u.Status, u.FilesAnalyzed)
	}
}

// A file someone else holds is left alone until their lease lapses; then it
// is taken, and their stopping counts as an attempt on it.
func TestALapsedClaimIsTakenAndCounts(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, quietFile)
	f.claim(owlFile, "worker-gone", testNow.Add(time.Minute))

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if owl := f.file(owlFile); owl.Status != db.AudioAnalyzing || owl.ClaimedBy != "worker-gone" {
		t.Fatalf("held file = %s, claimed by %q; want it left to its worker", owl.Status, owl.ClaimedBy)
	}
	if f.file(quietFile).Status != db.AudioAnalyzed {
		t.Error("the file nobody held wasn't analyzed")
	}
	if u := f.upload(); u.Status != db.StatusProcessing {
		t.Errorf("card = %s, want processing while a file is held", u.Status)
	}
	if want := testNow.Add(time.Minute); !f.queue.nextLapse.Equal(want) {
		t.Errorf("next lapse = %v, want %v, so Run looks again then", f.queue.nextLapse, want)
	}

	// The lease runs out.
	f.queue.now = func() time.Time { return testNow.Add(2 * time.Minute) }
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if owl := f.file(owlFile); owl.Status != db.AudioAnalyzed || owl.Attempts != 0 || owl.ClaimedBy != "" {
		t.Errorf("lapsed file = %s, %d attempts, claimed by %q; want analyzed and let go", owl.Status, owl.Attempts, owl.ClaimedBy)
	}
}

// A file whose worker keeps dying on it fails rather than taking down every
// worker that picks it up.
func TestAFileThatKeepsLosingItsWorkerFails(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		f.claim(owlFile, "worker-gone", testNow.Add(-time.Second))
		if _, err := f.store.UpdateAudioFile(t.Context(), ref, db.AudioFileID(ref, owlFile), func(a *db.AudioFile) error {
			a.Attempts = attempt - 1
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if attempt < maxAttempts {
			continue
		}
		if err := f.queue.drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	owl := f.file(owlFile)
	if owl.Status != db.AudioFailed || owl.Attempts != maxAttempts || owl.ClaimedBy != "" || len(f.bird.calls) != 0 {
		t.Errorf("file = %s (%q), %d attempts, %d runs; want failed without running", owl.Status, owl.StatusDetail, owl.Attempts, len(f.bird.calls))
	}
	if u := f.upload(); u.Status != db.StatusNeedsAttention {
		t.Errorf("card = %s, want needs_attention", u.Status)
	}
}

// A run whose claim goes to someone else stops, and leaves the file to them
// without counting an attempt.
func TestARunThatLosesItsClaimStops(t *testing.T) {
	f := newFixture(t)
	f.bird.running = make(chan struct{})
	f.queue.renewEvery = time.Millisecond
	f.card(db.StatusProcessing, owlFile)

	done := make(chan error)
	go func() { done <- f.queue.drain(t.Context()) }()
	<-f.bird.running
	if owl := f.file(owlFile); owl.ClaimedBy != f.queue.worker || owl.LeaseUntil == nil || !owl.LeaseUntil.Equal(testNow.Add(leaseFor)) {
		t.Fatalf("running file claimed by %q until %v; want this worker, a lease from now", owl.ClaimedBy, owl.LeaseUntil)
	}
	f.claim(owlFile, "worker-b", testNow.Add(leaseFor))

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run didn't stop when its claim went to someone else")
	}
	if owl := f.file(owlFile); owl.Status != db.AudioAnalyzing || owl.ClaimedBy != "worker-b" || owl.Attempts != 0 {
		t.Errorf("file = %s, claimed by %q, %d attempts; want it left to worker-b as it was", owl.Status, owl.ClaimedBy, owl.Attempts)
	}
}

// A queue that is stopped lets go of the file it was running, so the next
// start takes it up at once, and it isn't counted against the file.
func TestStoppingLetsGoOfTheFile(t *testing.T) {
	f := newFixture(t)
	f.bird.running = make(chan struct{})
	f.card(db.StatusProcessing, owlFile)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- f.queue.drain(ctx) }()
	<-f.bird.running
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drain = %v, want it cancelled", err)
	}
	if owl := f.file(owlFile); owl.Status != db.AudioUploaded || owl.ClaimedBy != "" || owl.LeaseUntil != nil || owl.Attempts != 0 {
		t.Errorf("file = %s, claimed by %q until %v, %d attempts; want queued again, unclaimed", owl.Status, owl.ClaimedBy, owl.LeaseUntil, owl.Attempts)
	}
}
