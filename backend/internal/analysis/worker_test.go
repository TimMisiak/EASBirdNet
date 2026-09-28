package analysis

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// A worker analyzes what there is, publishes where it stands, and exits
// once there has been nothing to claim for its idle time.
func TestAWorkerRunsUntilIdle(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, quietFile)
	f.queue.OnStatus = PublishStatus(f.files, f.queue.log)

	done := make(chan error)
	go func() { done <- f.queue.RunUntilIdle(t.Context(), 50*time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker didn't exit once idle")
	}
	if u := f.upload(); u.Status != db.StatusInReview {
		t.Errorf("card = %s, want in_review", u.Status)
	}
	s, ok, err := ReadStatus(t.Context(), f.files)
	if err != nil || !ok || s.State != StateReady {
		t.Errorf("published status = %+v, %v, %v; want ready", s, ok, err)
	}
}

// A worker whose models can't run says so and exits at once, rather than
// holding a replica checking for good.
func TestAWorkerWithoutModelsExits(t *testing.T) {
	f := newFixture(t)
	f.bird.check = func(int) error { return errors.New("no module named birdnet") }
	f.queue.OnStatus = PublishStatus(f.files, f.queue.log)
	if err := f.queue.RunUntilIdle(t.Context(), time.Minute); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("RunUntilIdle = %v, want ErrUnavailable", err)
	}
	if s, _, _ := ReadStatus(t.Context(), f.files); s.State != StateUnavailable || s.Detail != "no module named birdnet" {
		t.Errorf("published status = %+v", s)
	}
}

type fakeLauncher struct {
	mu      sync.Mutex
	running int
	started int
	err     error
}

func (l *fakeLauncher) Running(context.Context) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running, l.err
}

func (l *fakeLauncher) Start(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.started++
	return nil
}

// The web app starts a worker for every so many steps waiting, up to its
// cap, counting those already running and those it has just started.
func TestTheDispatcherSizesTheJob(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.card(db.StatusProcessing, owlFile, quietFile, brokenFile) // three BirdNET steps
	l := &fakeLauncher{running: 1}
	d := NewDispatcher(f.queue, l, f.files, f.queue.log)
	d.StepsPerWorker, d.MaxWorkers = 1, 10

	d.dispatch(t.Context())
	if l.started != 2 {
		t.Errorf("started %d, want 2 beside the one running for three steps", l.started)
	}
	// Azure hasn't caught up with those two: nothing more starts.
	d.dispatch(t.Context())
	if l.started != 2 {
		t.Errorf("started %d after a second look, want no more", l.started)
	}

	d.MaxWorkers = 1
	d.started = nil
	l.started = 0
	d.dispatch(t.Context())
	if l.started != 0 {
		t.Errorf("started %d with the cap already running", l.started)
	}

	// Failing to start is what a coordinator is shown, until a start works.
	d.MaxWorkers, l.running, l.err = 10, 0, errors.New("403 AuthorizationFailed")
	d.dispatch(t.Context())
	if s := d.Status(); s.State != StateFailing || s.Detail != "counting the running analysis workers: 403 AuthorizationFailed" {
		t.Errorf("status = %+v, want the failure to start", s)
	}
	l.err = nil
	d.dispatch(t.Context())
	if s := d.Status(); s.State != StateIdle {
		t.Errorf("status = %+v, want idle again once starting works", s)
	}
}

// With nothing waiting, nothing starts.
func TestTheDispatcherStartsNothingForNothing(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusInReview, owlFile)
	l := &fakeLauncher{}
	NewDispatcher(f.queue, l, f.files, f.queue.log).dispatch(t.Context())
	if l.started != 0 {
		t.Errorf("started %d with nothing waiting", l.started)
	}
}
