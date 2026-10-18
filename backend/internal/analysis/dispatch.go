package analysis

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// Launcher starts analysis workers somewhere else: executions of a Container
// Apps job (internal/azjob), each one worker replica.
type Launcher interface {
	// Running is how many workers are running now, or starting.
	Running(ctx context.Context) (int, error)
	// Start starts one more.
	Start(ctx context.Context) error
}

// Dispatcher is the web app's side of analysis run as a job. It stands where
// a Queue would in the API: Enqueue, when a card's last file lands, starts
// workers for the work waiting; a backstop does the same every few minutes,
// for a start that failed, a worker that died, or work waiting on a retry;
// and Status is what the workers last published.
type Dispatcher struct {
	queue    *Queue // only for Pending: its store and whether Perch is on
	launcher Launcher
	files    storage.Store
	log      *slog.Logger

	// MaxWorkers caps the workers running at once, and StepsPerWorker is how
	// much waiting work justifies one more: a card of 300 files with Perch on
	// is 600 steps, so 25 a worker asks for the cap on a whole card and one
	// worker for a few stragglers.
	MaxWorkers     int
	StepsPerWorker int
	// Backstop is how often it looks even when nothing woke it.
	Backstop time.Duration

	wake chan struct{}

	mu sync.Mutex
	// published is the workers' last status; failed, when set, is the
	// dispatcher's own failure to start them, which a coordinator needs to
	// see over anything a worker said.
	published Status
	failed    *Status
	// started are the starts not yet old enough to be sure Running counts
	// them: the job's execution list can lag a start.
	started []time.Time
}

// NewDispatcher returns a dispatcher that sizes the work by queue's store
// and Perch setting and starts workers with launcher.
func NewDispatcher(queue *Queue, launcher Launcher, files storage.Store, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		queue: queue, launcher: launcher, files: files, log: log,
		MaxWorkers: 10, StepsPerWorker: 25, Backstop: 5 * time.Minute,
		wake:      make(chan struct{}, 1),
		published: Status{State: StateIdle},
	}
}

// StateIdle is analysis run as a job with no worker having reported yet.
const StateIdle = "idle"

// startLag is how long a start is counted as running whatever the job's
// execution list says, which can lag it.
const startLag = 3 * time.Minute

// Enqueue wakes the dispatcher to start workers for a card that has arrived.
// It never blocks.
func (d *Dispatcher) Enqueue(string) {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Status is what the workers last published, or the dispatcher's own failure
// to start them.
func (d *Dispatcher) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return *d.failed
	}
	return d.published
}

// Run dispatches at once, then whenever woken and on the backstop, and reads
// the workers' status every 30 seconds, until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	backstop := time.NewTicker(d.Backstop)
	defer backstop.Stop()
	refresh := time.NewTicker(30 * time.Second)
	defer refresh.Stop()
	d.refresh(ctx)
	d.dispatch(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			d.refresh(ctx)
		case <-backstop.C:
			d.dispatch(ctx)
		case <-d.wake:
			d.dispatch(ctx)
		}
	}
}

func (d *Dispatcher) refresh(ctx context.Context) {
	s, ok, err := ReadStatus(ctx, d.files)
	if err != nil {
		d.log.Warn("analysis: reading the workers' status", "err", err)
		return
	}
	if ok {
		d.mu.Lock()
		d.published = s
		d.mu.Unlock()
	}
}

// dispatch starts workers until there is one for every StepsPerWorker steps
// waiting, up to MaxWorkers, counting those already running.
func (d *Dispatcher) dispatch(ctx context.Context) {
	pending, err := d.queue.Pending(ctx)
	if err != nil {
		d.log.Warn("analysis: counting the work waiting", "err", err)
		return
	}
	if pending == 0 {
		d.clearFailure()
		return
	}
	running, err := d.launcher.Running(ctx)
	if err != nil {
		d.fail("counting the running analysis workers", err)
		return
	}
	now := time.Now()
	d.mu.Lock()
	recent := d.started[:0]
	for _, t := range d.started {
		if now.Sub(t) < startLag {
			recent = append(recent, t)
		}
	}
	d.started = recent
	// Counted twice once the list has caught up with them: too few workers
	// started for a few minutes, which the backstop makes up, rather than
	// too many.
	running += len(recent)
	d.mu.Unlock()

	want := min(max(d.MaxWorkers, 1), (pending+max(d.StepsPerWorker, 1)-1)/max(d.StepsPerWorker, 1))
	for i := running; i < want; i++ {
		if err := d.launcher.Start(ctx); err != nil {
			d.fail("starting an analysis worker", err)
			return
		}
		d.mu.Lock()
		d.started = append(d.started, now)
		d.mu.Unlock()
	}
	if want > running {
		d.log.Info("analysis: started workers", "started", want-running, "running", running, "steps_waiting", pending)
	}
	d.clearFailure()
}

func (d *Dispatcher) fail(what string, err error) {
	d.log.Error("analysis: "+what, "err", err)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Second)
	s := Status{State: StateFailing, Detail: what + ": " + err.Error(), Since: now, CheckedAt: now}
	if d.failed != nil {
		s.Since = d.failed.Since
	}
	d.failed = &s
}

func (d *Dispatcher) clearFailure() {
	d.mu.Lock()
	d.failed = nil
	d.mu.Unlock()
}
