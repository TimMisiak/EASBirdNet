// Package analysis runs BirdNET over the audio of received cards and stores
// what it hears, and, when Queue.Perch is set, runs Google's Perch v2 over
// the same files as a second opinion.
//
// The queue is the database. A card whose last file has landed is
// "processing", and each of its files in "uploaded" status is waiting for
// BirdNET. Queue.Run works through them, as many at once as its Capacity
// holds, oldest card first. For each, it copies the file out of storage, runs
// internal/birdnet over it, merges
// the consecutive windows in which one species was heard (merge.go), cuts a
// clip of each detection into storage, writes the detections, and marks the
// file analyzed.
// With Perch on, a file BirdNET has analyzed is then queued for Perch, which
// goes through the same steps and stores its detections beside BirdNET's,
// each marked with the model that heard it. It is a step of its own
// (db.AudioFile.Perch), and a free slot goes to a BirdNET file before a Perch
// one, so a card waiting for BirdNET never waits behind Perch; Perch runs
// beside BirdNET where its memory fits (fill). Perch failing on a file leaves
// BirdNET's result on it as it was.
// When no file on a card is left for either, the card moves on to in_review,
// or to needs_attention if BirdNET couldn't read some of it.
//
// Because the state lives in the Store rather than in memory, a restart (a
// deploy, a crash, a replica scaled in) loses nothing: the next Run picks up
// the files still waiting. A file is claimed before it is run (db.Claim: take,
// hold, release), so more than one worker can share the queue: a worker that
// is stopped lets go of its file, and one that dies holding it holds it only
// until its lease lapses, which counts as an attempt on the file. Detection
// ids are derived from what was heard, so running a file twice overwrites its
// detections rather than duplicating them.
//
// Run won't start until BirdNET answers a Check, and keeps checking until it
// does. What it is waiting for, or what a pass last failed on, is Status,
// which the admin API serves: a card stuck in processing should say why on the
// screen that lists it.
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
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/perf"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// Analyzer is what runs BirdNET and cuts clips: birdnet.Analyzer, or a fake in
// tests.
type Analyzer interface {
	// Check reports whether Analyze and Cut can run at all. The queue calls it
	// before it starts, and again until it passes.
	Check(ctx context.Context) error
	// CheckPerch reports whether Analyze can run birdnet.ModelPerch too. The
	// queue calls it instead of Check when Perch is on.
	CheckPerch(ctx context.Context) error
	Analyze(ctx context.Context, paths []string, opts birdnet.Options) (birdnet.Result, error)
	Cut(ctx context.Context, source string, clips []birdnet.Clip) (birdnet.Recording, error)
}

// Settings every card is analyzed with. They are recorded on the card
// (db.Analysis), so a detection can be traced back to them.
var settings = birdnet.Options{
	MinConfidence: birdnet.DefaultMinConfidence,
	Sensitivity:   birdnet.DefaultSensitivity,
	OverlapSec:    0,
}

const (
	// maxAttempts is how many times a file is tried when analyzing it fails in
	// a way that might pass -- BirdNET crashing or being killed, or storing
	// what it heard failing -- rather than reporting the file as unreadable.
	// After that the file is failed, so one bad file can't hold up every card
	// behind it.
	maxAttempts = 3
	// retryDelay is the first wait after such a failure. It doubles up to
	// maxRetryDelay for as long as passes keep failing, and a pass that
	// doesn't puts it back. So a file that keeps failing is tried again 30 s
	// and then 60 s later, while an outage that fails every pass -- the store
	// or storage being unreachable, rather than one file -- backs off instead
	// of being retried every 30 s for as long as it lasts.
	retryDelay    = 30 * time.Second
	maxRetryDelay = 10 * time.Minute
	// checkDelay is the first wait between BirdNET checks while it can't run,
	// doubling up to maxCheckDelay. checkTimeout bounds one check.
	checkDelay    = 30 * time.Second
	maxCheckDelay = 10 * time.Minute
	checkTimeout  = time.Minute
	// leaseFor is how long a claim on a file holds unless it is renewed, and
	// renewEvery how often a worker running the file renews it. A worker that
	// dies holding a file keeps it from everyone for at most leaseFor; a
	// worker that is stopped lets go of it at once (release).
	leaseFor   = 5 * time.Minute
	renewEvery = time.Minute
	// releaseTimeout bounds letting go of a file on the way out, which has to
	// happen after the context that ran it has ended.
	releaseTimeout = 10 * time.Second
)

// What Status.State can be.
const (
	// StateStarting is before the queue has established anything, which lasts
	// as long as the first BirdNET check.
	StateStarting = "starting"
	// StateReady is BirdNET running and the cards moving.
	StateReady = "ready"
	// StateUnavailable is BirdNET not running here at all: the scripts or the
	// Python are missing or wrong, so every card waits in processing.
	StateUnavailable = "unavailable"
	// StateFailing is BirdNET running but a pass over the cards stopping on
	// something else -- the store or storage -- which the queue is retrying.
	StateFailing = "failing"
)

// Status is why the queue is or isn't working through cards. Nothing stores
// it: it is this process's own state, and what a coordinator is shown against
// a card sitting in processing, so a stuck card gives a reason without anyone
// reading container logs.
type Status struct {
	// State is one of the State* constants above.
	State string
	// Detail is what went wrong, when State isn't ready. It names server-side
	// paths and commands, so it is for coordinators, not volunteers.
	Detail string
	// Since is when the queue entered this state, and CheckedAt when it last
	// confirmed it.
	Since, CheckedAt time.Time
}

// Queue works through received cards. Create it with New and start it with
// Run; Enqueue tells it a card has arrived.
type Queue struct {
	store    db.Store
	files    storage.Store
	analyzer Analyzer
	log      *slog.Logger

	// Perch runs Perch over each file after BirdNET. Set it before Run.
	Perch bool
	// Perf, if set, records what each model's pass over a file cost:
	// time per phase, CPU and memory (ANALYSIS.md, *Performance data*).
	// Set it before Run.
	Perf *perf.Recorder

	wake chan struct{}
	// worker is what this queue's claims on files are made in the name of
	// (db.Claim.ClaimedBy): the process, which is the replica in Azure.
	worker string
	// Capacity is how much the queue runs at once, and Costs what each
	// model's task is expected to take (DefaultCosts where a model has none).
	// Set them before Run.
	Capacity Capacity
	Costs    map[string]Cost

	// nextWake is the soonest a waiting file becomes claimable -- its
	// retryAfter passes, or someone else's claim on it lapses -- as the last
	// scan found it. Run looks again then, since nothing else will wake it.
	// Only the goroutine running drain touches it.
	nextWake time.Time
	// tallyMu serializes recounting cards, which finishing tasks do at once.
	tallyMu sync.Mutex

	// mu guards status, which Run writes and Status reads from whatever
	// goroutine an HTTP handler is on.
	mu     sync.Mutex
	status Status

	// Clock and waits, so tests don't sleep.
	now        func() time.Time
	retryDelay time.Duration
	checkDelay time.Duration
	renewEvery time.Duration
	// pause is sleep, so a test can see how long Run waited without waiting.
	pause func(context.Context, time.Duration) bool
}

// New returns a queue that reads audio from files and writes to store.
func New(store db.Store, files storage.Store, analyzer Analyzer, log *slog.Logger) *Queue {
	q := &Queue{
		store: store, files: files, analyzer: analyzer, log: log,
		wake:     make(chan struct{}, 1),
		Capacity: Capacity{Cores: 1, MaxTasks: 1},
		worker:   fmt.Sprintf("%s:%d", perf.Instance(), os.Getpid()),
		now:      time.Now, retryDelay: retryDelay, checkDelay: checkDelay, renewEvery: renewEvery,
		pause: sleep,
	}
	q.status = Status{State: StateStarting, Since: q.stamp(), CheckedAt: q.stamp()}
	return q
}

// Status reports why the queue is or isn't working through cards. It is safe
// to call from any goroutine, and on a queue whose Run isn't started.
func (q *Queue) Status() Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.status
}

// setStatus records where the queue stands. Since only moves when the state
// itself does, so "unavailable since" is when it broke, not when it was last
// looked at.
func (q *Queue) setStatus(state, detail string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.stamp()
	if q.status.State != state {
		q.status.Since = now
	}
	// Trimmed because a subprocess's error ends in a newline, and this is read
	// on a screen.
	q.status.State, q.status.Detail, q.status.CheckedAt = state, strings.TrimSpace(detail), now
}

// Enqueue tells the queue a card has been received. It never blocks: the card
// is already queued by its status, and this only wakes Run to look.
func (q *Queue) Enqueue(reference string) {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run analyzes queued files until ctx is cancelled. It waits for BirdNET to be
// available, works through whatever was left queued, then waits for Enqueue.
// Cancelling ctx kills a BirdNET run in flight; its file is run again next
// time.
//
// A pass that fails pauses the queue and is tried again on a growing delay
// (see retryDelay), because the two things that fail a whole pass are a file
// worth another attempt and an outage worth waiting out.
func (q *Queue) Run(ctx context.Context) {
	if !q.waitReady(ctx) {
		return
	}
	delay := q.retryDelay
	for {
		err := q.drain(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, errRetry):
			// Files that will be tried again at their retryAfter, which
			// idle waits for; everything else went on meanwhile.
			q.setStatus(StateFailing, err.Error())
			q.log.Warn("analysis: files will be tried again", "err", err)
			delay = q.retryDelay
		case err != nil:
			q.setStatus(StateFailing, err.Error())
			q.log.Error("analysis: pausing after a failure", "err", err, "retry_in", delay)
			if !q.pause(ctx, delay) {
				return
			}
			delay = min(2*delay, maxRetryDelay)
			continue
		default:
			delay = q.retryDelay
			q.setStatus(StateReady, "")
		}
		if !q.idle(ctx) {
			return
		}
	}
}

// idle waits for Enqueue, or for the soonest waiting file to become
// claimable, and reports false if ctx ended first.
func (q *Queue) idle(ctx context.Context) bool {
	var lapse <-chan time.Time
	if !q.nextWake.IsZero() {
		// A second past it, so it has passed by the store's clock as well as
		// this one.
		t := time.NewTimer(max(q.nextWake.Sub(q.now()), 0) + time.Second)
		defer t.Stop()
		lapse = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-q.wake:
	case <-lapse:
	}
	return true
}

// waitReady blocks until BirdNET can run, checking again on a delay that grows
// to maxCheckDelay, and reports whether it got there before ctx was cancelled.
//
// The check lives here rather than at startup because a server that can't run
// BirdNET is a server whose cards pile up in processing with nothing to show
// for it: checking on a loop means the warning keeps being logged for as long
// as it is true, Status can say so on the coordinator's screens, and an
// environment put right underneath a running server (a mounted venv, a model
// download that hadn't finished) is picked up without a restart.
func (q *Queue) waitReady(ctx context.Context) bool {
	delay := q.checkDelay
	for {
		checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
		check := q.analyzer.Check
		if q.Perch {
			// Perch was asked for, so a server that can't run it says so
			// rather than finishing cards without it.
			check = q.analyzer.CheckPerch
		}
		err := check(checkCtx)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			q.setStatus(StateReady, "")
			q.log.Info("analysis: BirdNET is available; working through received cards", "perch", q.Perch)
			return true
		}
		q.setStatus(StateUnavailable, err.Error())
		q.log.Warn("BirdNET isn't available, so received cards will wait in processing; set BIRDSENSE_BIRDNET_PYTHON and BIRDSENSE_BIRDNET_SCRIPT (and install analyzer/requirements-perch.txt, or turn BIRDSENSE_PERCH off, if Perch is on)",
			"err", err, "perch", q.Perch, "retry_in", delay)
		if !sleep(ctx, delay) {
			return false
		}
		delay = min(2*delay, maxCheckDelay)
	}
}

// errRetry is an attempt on a file that failed and is worth trying again. The
// file waits until its retryAfter; the queue goes on with everything else.
var errRetry = errors.New("analysis: attempt failed")

// Capacity is what the queue may run at once. The zero Capacity runs one task
// at a time with no memory budget, which is what New gives.
type Capacity struct {
	// Cores is how much CPU the tasks may keep busy together. A task takes a
	// core per thread (Perch) or per worker (BirdNET). It is a target, not a
	// limit: running over it only slows every task down.
	Cores float64
	// Memory is what the tasks may hold together, in bytes: the replica's
	// memory less what the server itself needs. It is a limit: a task starts
	// only if its model's Cost.Memory fits beside the running ones, because
	// running over it gets the replica killed with every file on it. 0 is no
	// budget. A task that fits nowhere still runs when nothing else is.
	Memory int64
	// MaxTasks caps the tasks running at once; less than 1 is 1.
	MaxTasks int
}

// Cost is what one task of a model is expected to take.
type Cost struct {
	// Memory is the task's peak, with a margin: measured, the peak PSS of the
	// model's whole process tree (ANALYSIS.md, *Measured*).
	Memory int64
	// Threads is how many threads a Perch task's TensorFlow runs on; 0 gives
	// it the cores free when it starts, at least one. BirdNET's interpreter is
	// single-threaded, so a BirdNET task takes one core whatever this says.
	Threads int
}

// DefaultCosts are the measured peaks, with a margin: BirdNET ~260 MB and
// Perch ~1.95 GB of PSS on a real card.
var DefaultCosts = map[string]Cost{
	birdnet.ModelBirdNET: {Memory: 350 << 20},
	birdnet.ModelPerch:   {Memory: 2300 << 20},
}

// steps are the models that run over every file, first to last. The order is
// the priority: a slot goes to the first step with a file waiting, so a new
// card's BirdNET never waits behind an old card's Perch.
func (q *Queue) steps() []step {
	if q.Perch {
		return []step{birdnetStep, perchStep}
	}
	return []step{birdnetStep}
}

func (q *Queue) cost(model string) Cost {
	if c, ok := q.Costs[model]; ok {
		return c
	}
	return DefaultCosts[model]
}

// A task is one step over one file, run in a goroutine of its own.
type task struct {
	card    db.Upload
	file    db.AudioFile
	step    step
	threads int
	cores   float64
	memory  int64
}

type taskKey struct{ file, model string }

func (t *task) key() taskKey { return taskKey{t.file.ID, t.step.model} }

type outcome struct {
	task *task
	err  error
}

// drain runs every claimable step of every processing card, as many at once
// as Capacity allows, and finishes each card when nothing is left on it. It
// returns once nothing is running and nothing more can start: nil, or the
// retries it met (errRetry), or the first other failure -- the store, most
// likely -- which stops it starting anything new, and which Run pauses for.
func (q *Queue) drain(ctx context.Context) error {
	running := map[taskKey]*task{}
	results := make(chan outcome)
	var retries []error
	var stop error
	scan := true
	for {
		if scan && stop == nil && ctx.Err() == nil {
			scan = false
			// Wake-ups that arrive now are covered by this scan, as long as
			// the cards are read after them.
			select {
			case <-q.wake:
			default:
			}
			if err := q.fill(ctx, running, results); err != nil {
				stop = err
			}
		}
		if len(running) == 0 {
			if stop != nil {
				return stop
			}
			return errors.Join(retries...)
		}
		select {
		case o := <-results:
			delete(running, o.task.key())
			switch err := q.gone(ctx, o.task.card, o.err); {
			case err == nil:
			case errors.Is(err, errRetry):
				retries = append(retries, err)
			case stop == nil:
				stop = err
			}
			scan = true
		case <-q.wake:
			scan = true
		case <-ctx.Done():
			// The running tasks see it too, and let go of their files.
			for len(running) > 0 {
				o := <-results
				delete(running, o.task.key())
			}
			return ctx.Err()
		}
	}
}

// fill looks at every processing card and starts what fits beside the running
// tasks: the first step's files before the next step's, oldest card first. A
// card with nothing waiting and nothing running is tallied, which finishes it
// if its last file was done before a restart got to finishing it. It notes
// the soonest a waiting file becomes claimable (nextWake), for Run.
func (q *Queue) fill(ctx context.Context, running map[taskKey]*task, results chan<- outcome) error {
	cards, err := q.store.ListUploads(ctx, db.UploadFilter{Status: db.StatusProcessing})
	if err != nil {
		return err
	}
	// First come, first analyzed.
	slices.SortStableFunc(cards, func(a, b db.Upload) int { return receivedAt(a).Compare(receivedAt(b)) })
	q.nextWake = time.Time{}
	steps := q.steps()
	type candidate struct {
		card int
		file db.AudioFile
		step step
	}
	byStep := make([][]candidate, len(steps))
	for i, card := range cards {
		files, err := q.store.ListAudioFiles(ctx, card.ID)
		if err := q.gone(ctx, card, err); err != nil {
			return err
		}
		busy := false
		for _, f := range files {
			if !q.Perch && perchWaiting(f) {
				// Queued for Perch while it was on, and it is off now. The
				// file never got its second opinion, so it says none.
				if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
					if perchWaiting(*f) {
						f.Perch = nil
					}
					return nil
				}); err != nil {
					return err
				}
				f.Perch = nil
			}
			for j, s := range steps {
				if running[taskKey{f.ID, s.model}] != nil {
					busy = true
					continue
				}
				ok, wake := q.claimable(f, s)
				if ok {
					byStep[j] = append(byStep[j], candidate{i, f, s})
					busy = true
				} else if !wake.IsZero() && (q.nextWake.IsZero() || wake.Before(q.nextWake)) {
					q.nextWake = wake
				}
			}
		}
		if !busy {
			if err := q.gone(ctx, card, q.tally(ctx, card.ID)); err != nil {
				return err
			}
		}
	}

	most := max(q.Capacity.MaxTasks, 1)
	for _, candidates := range byStep {
		for _, c := range candidates {
			if len(running) >= most {
				return nil
			}
			var memory int64
			var cores float64
			for _, t := range running {
				memory, cores = memory+t.memory, cores+t.cores
			}
			cost := q.cost(c.step.model)
			if q.Capacity.Memory > 0 && memory+cost.Memory > q.Capacity.Memory && len(running) > 0 {
				continue
			}
			t := &task{file: c.file, step: c.step, memory: cost.Memory, cores: 1}
			if c.step.model == birdnet.ModelPerch {
				t.threads = cost.Threads
				if t.threads == 0 {
					t.threads = max(1, int(q.Capacity.Cores-cores))
				}
				t.cores = float64(t.threads)
			}
			if cards[c.card].Analysis == nil {
				if cards[c.card], err = q.startCard(ctx, cards[c.card].ID); err != nil {
					return q.gone(ctx, cards[c.card], err)
				}
			}
			t.card = cards[c.card]
			running[t.key()] = t
			go func() { results <- outcome{t, q.runTask(ctx, t)} }()
		}
	}
	return nil
}

// runTask runs one step over one file, and recounts its card.
func (q *Queue) runTask(ctx context.Context, t *task) error {
	var err error
	if t.step.model == birdnet.ModelPerch {
		err = q.perchFile(ctx, t.card, t.file, t.threads)
	} else {
		err = q.analyzeFile(ctx, t.card, t.file)
	}
	if err != nil {
		return err
	}
	return q.tally(ctx, t.card.ID)
}

// gone passes on what processing a card returned, unless it is the card
// having been deleted from under the queue. Only deleting a card removes its
// documents, so that is a card being deleted, and detections stored for it
// after the delete swept past them go now.
func (q *Queue) gone(ctx context.Context, card db.Upload, err error) error {
	if !errors.Is(err, db.ErrNotFound) {
		return err
	}
	q.log.Info("analysis: card deleted while being analyzed", "upload", card.ID)
	if err := q.store.DeleteUpload(ctx, card.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
		return err
	}
	return nil
}

func receivedAt(u db.Upload) time.Time {
	if u.ReceivedAt != nil {
		return *u.ReceivedAt
	}
	return u.UpdatedAt
}

// queued reports whether a file is in BirdNET's queue: waiting for it, or
// being run by someone (claimable says whether it can be taken).
func queued(f db.AudioFile) bool {
	return f.Status == db.AudioUploaded || f.Status == db.AudioAnalyzing
}

// perchWaiting reports whether a file is in Perch's queue, the same way.
func perchWaiting(f db.AudioFile) bool {
	return f.Perch != nil && (f.Perch.Status == db.PerchQueued || f.Perch.Status == db.PerchAnalyzing)
}

// startCard records the settings a card is analyzed with.
func (q *Queue) startCard(ctx context.Context, id string) (db.Upload, error) {
	started := q.stamp()
	return q.store.UpdateUpload(ctx, id, func(u *db.Upload) error {
		if u.Analysis == nil {
			u.Analysis = &db.Analysis{
				MinConfidence: settings.MinConfidence, Sensitivity: settings.Sensitivity,
				OverlapSec: settings.OverlapSec, StartedAt: started,
			}
		}
		return nil
	})
}

// A step is one model's pass over a file. BirdNET's is the file's own status;
// Perch's is its Perch field, so a second opinion that fails leaves the first
// as it was.
type step struct {
	model string // birdnet.ModelBirdNET or birdnet.ModelPerch
	// waiting reports whether the file is in this step's queue, running or
	// not; running, whether someone has it ("analyzing").
	waiting, running func(f db.AudioFile) bool
	// claim is where the step's claim is on the file. Only called on a file
	// the step is waiting for.
	claim func(f *db.AudioFile) *db.Claim
	// begin marks a waiting file as being run.
	begin func(f *db.AudioFile)
	// requeue puts a file this step was cut off on back in its queue.
	requeue func(f *db.AudioFile)
	// failed records that this step can't be done on the file, and why.
	failed func(f *db.AudioFile, why string, at time.Time)
}

var birdnetStep = step{
	model:   birdnet.ModelBirdNET,
	waiting: queued,
	running: func(f db.AudioFile) bool { return f.Status == db.AudioAnalyzing },
	claim:   func(f *db.AudioFile) *db.Claim { return &f.Claim },
	begin:   func(f *db.AudioFile) { f.Status, f.StatusDetail = db.AudioAnalyzing, "" },
	requeue: func(f *db.AudioFile) {
		if f.Status == db.AudioAnalyzing {
			f.Status = db.AudioUploaded
		}
		f.ClaimedBy, f.LeaseUntil = "", nil
	},
	failed: func(f *db.AudioFile, why string, at time.Time) {
		f.Status, f.StatusDetail, f.AnalyzedAt, f.DetectionCount = db.AudioFailed, why, &at, 0
		f.ClaimedBy, f.LeaseUntil, f.RetryAfter = "", nil, nil
	},
}

var perchStep = step{
	model:   birdnet.ModelPerch,
	waiting: perchWaiting,
	running: func(f db.AudioFile) bool { return f.Perch != nil && f.Perch.Status == db.PerchAnalyzing },
	claim:   func(f *db.AudioFile) *db.Claim { return &f.Perch.Claim },
	begin:   func(f *db.AudioFile) { f.Perch.Status, f.Perch.StatusDetail = db.PerchAnalyzing, "" },
	requeue: func(f *db.AudioFile) {
		if f.Perch != nil && f.Perch.Status == db.PerchAnalyzing {
			f.Perch.Status = db.PerchQueued
		}
		if f.Perch != nil {
			f.Perch.ClaimedBy, f.Perch.LeaseUntil = "", nil
		}
	},
	failed: func(f *db.AudioFile, why string, at time.Time) {
		var attempts int
		if f.Perch != nil {
			attempts = f.Perch.Attempts
		}
		f.Perch = &db.PerchRun{Status: db.PerchFailed, StatusDetail: why, AnalyzedAt: &at, Claim: db.Claim{Attempts: attempts}}
	},
}

// claimable reports whether a step of a file is there to be taken: waiting
// for it, not being run, and past any retryAfter; or being run on a claim
// that has lapsed. When it is waiting but not yet claimable, wake is when it
// will be.
func (q *Queue) claimable(f db.AudioFile, s step) (ok bool, wake time.Time) {
	if !s.waiting(f) {
		return false, time.Time{}
	}
	c := s.claim(&f)
	now := q.now()
	if !s.running(f) {
		if c.RetryAfter != nil && now.Before(*c.RetryAfter) {
			return false, *c.RetryAfter
		}
		return true, time.Time{}
	}
	if c.LeaseUntil == nil || !now.Before(*c.LeaseUntil) {
		return true, time.Time{}
	}
	return false, *c.LeaseUntil
}

// take claims a step of a file for this worker, in one replace-if-unchanged
// write, and reports whether it got it. It doesn't when the step is no longer
// there to take -- someone else claimed it first, or it is done -- or when
// the claim that had lapsed on it was the file's last attempt, which fails it.
//
// A lapsed claim is a worker that stopped without letting go: killed, out of
// memory, or cut off from the store. That counts as an attempt, or a file
// that takes its worker down with it would be taken up again for ever. A step
// found running with no claim at all is from before claims, and was cut off
// by a restart: it is taken again, as it always was.
func (q *Queue) take(ctx context.Context, card db.Upload, f db.AudioFile, s step) (db.AudioFile, bool, error) {
	lease := q.now().UTC().Add(leaseFor)
	exhausted := false
	f, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		if ok, _ := q.claimable(*f, s); !ok {
			return errSkip
		}
		c := s.claim(f)
		if s.running(*f) && c.ClaimedBy != "" {
			c.Attempts++
			if c.Attempts >= maxAttempts {
				exhausted = true
				s.failed(f, fmt.Sprintf("analysis stopped part way through this file %d times: its worker was stopped or lost its claim", c.Attempts), q.stamp())
				return nil
			}
		}
		s.begin(f)
		c = s.claim(f)
		c.ClaimedBy, c.LeaseUntil, c.RetryAfter = q.worker, &lease, nil
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return f, false, nil
	case err != nil:
		return f, false, err
	case exhausted:
		q.log.Error("analysis: giving up on a file whose worker kept stopping", "upload", card.ID, "path", f.Path, "model", s.model)
		return f, false, nil
	}
	return f, true, nil
}

// errLostClaim is a run cut short because the file's claim is no longer this
// worker's.
var errLostClaim = errors.New("analysis: lost the claim on the file")

// hold renews this worker's claim on a step every renewEvery while it runs,
// and returns a context for the run and a func that stops renewing. The
// context ends with errLostClaim as its cause if the claim turns out to be
// someone else's -- it lapsed, and they took it -- or can't be renewed twice
// running, because by then it may lapse: two workers running one file would
// write the same detections, but only one should be counting attempts on it.
func (q *Queue) hold(ctx context.Context, f db.AudioFile, s step) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(q.renewEvery)
		defer t.Stop()
		failures := 0
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			lease := q.now().UTC().Add(leaseFor)
			_, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
				if !s.running(*f) || s.claim(f).ClaimedBy != q.worker {
					return errLostClaim
				}
				s.claim(f).LeaseUntil = &lease
				return nil
			})
			switch {
			case err == nil:
				failures = 0
			case errors.Is(err, db.ErrNotFound):
				// The card is being deleted; the run finds that out itself.
				return
			case errors.Is(err, errLostClaim):
				cancel(errLostClaim)
				return
			default:
				failures++
				q.log.Warn("analysis: renewing the claim on a file", "upload", f.UploadID, "path", f.Path, "model", s.model, "err", err)
				if failures >= 2 {
					cancel(errLostClaim)
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(stop)
		<-stopped
		cancel(nil)
	}
}

// release lets go of a step this worker was running when it was stopped, so
// the next start takes it up at once rather than waiting out the lease. It
// isn't an attempt: nothing went wrong with the file.
func (q *Queue) release(ctx context.Context, f db.AudioFile, s step) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		if !s.running(*f) || s.claim(f).ClaimedBy != q.worker {
			return errSkip
		}
		s.requeue(f)
		return nil
	})
	if err != nil && !errors.Is(err, errSkip) {
		q.log.Warn("analysis: letting go of a file on the way out; it waits for its claim to lapse", "upload", f.UploadID, "path", f.Path, "model", s.model, "err", err)
	}
}

// unreadable is what is wrong with a file itself, which no retry will change.
type unreadable string

func (u unreadable) Error() string { return string(u) }

// result is what one model's run over a file stored.
type result struct {
	// model is the model's own name for itself, e.g. "Perch_v2".
	model      string
	detections int
	recording  birdnet.Recording
	// start is when the recording began, when startKnown.
	start      time.Time
	startKnown bool
}

// analyzeFile runs BirdNET over one file and stores the result. It returns an
// error only for something that should pause the queue: the store failing, or
// an attempt that failed in a way that may pass. What is wrong with the file
// itself is recorded on the file, and an attempt that keeps failing runs out
// of tries (retryOrFail) rather than repeating for good.
func (q *Queue) analyzeFile(ctx context.Context, card db.Upload, f db.AudioFile) error {
	f, ok, err := q.take(ctx, card, f, birdnetStep)
	if err != nil || !ok {
		return err
	}
	log := q.log.With("upload", card.ID, "path", f.Path)
	log.Info("analysis: starting file")
	began := time.Now()

	held, stop := q.hold(ctx, f, birdnetStep)
	res, err := q.run(held, card, f, birdnetStep, 0)
	stop()
	if err != nil {
		return q.settle(ctx, held, birdnetStep, f, err)
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Status, f.StatusDetail = db.AudioAnalyzed, ""
		f.AnalyzedAt, f.DetectionCount = &analyzed, res.detections
		f.Claim = db.Claim{}
		f.DurationSec, f.SampleRate = res.recording.DurationSec, res.recording.SampleRate
		if f.RecordedAt == nil && res.startKnown {
			t := res.start.UTC()
			f.RecordedAt = &t
		}
		// Queued for Perch now BirdNET is done with it. A file BirdNET has
		// run over again is queued again, since its Perch detections are
		// overwritten the same way its own are.
		f.Perch = nil
		if q.Perch {
			f.Perch = &db.PerchRun{Status: db.PerchQueued}
		}
		return nil
	}); err != nil {
		return q.settle(ctx, held, birdnetStep, f, fmt.Errorf("marking the file analyzed: %w", err))
	}
	if res.model != "" && card.Analysis != nil && card.Analysis.Model == "" {
		if _, err := q.store.UpdateUpload(ctx, card.ID, func(u *db.Upload) error {
			if u.Analysis != nil && u.Analysis.Model == "" {
				u.Analysis.Model = res.model
			}
			return nil
		}); err != nil {
			return err
		}
	}
	log.Info("analysis: finished file", "detections", res.detections, "dur", time.Since(began).Round(time.Second))
	return nil
}

// perchFile runs Perch over one file BirdNET has analyzed, and stores what it
// heard beside BirdNET's detections. It returns an error the same way
// analyzeFile does, and records what goes wrong on the file's Perch step.
func (q *Queue) perchFile(ctx context.Context, card db.Upload, f db.AudioFile, threads int) error {
	f, ok, err := q.take(ctx, card, f, perchStep)
	if err != nil || !ok {
		return err
	}
	log := q.log.With("upload", card.ID, "path", f.Path)
	log.Info("analysis: starting Perch on file")
	began := time.Now()

	held, stop := q.hold(ctx, f, perchStep)
	res, err := q.run(held, card, f, perchStep, threads)
	stop()
	if err != nil {
		return q.settle(ctx, held, perchStep, f, err)
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Perch = &db.PerchRun{Status: db.PerchAnalyzed, AnalyzedAt: &analyzed, DetectionCount: res.detections}
		return nil
	}); err != nil {
		return q.settle(ctx, held, perchStep, f, fmt.Errorf("marking the file analyzed by Perch: %w", err))
	}
	if res.model != "" && card.Analysis != nil && card.Analysis.PerchModel == "" {
		if _, err := q.store.UpdateUpload(ctx, card.ID, func(u *db.Upload) error {
			if u.Analysis != nil && u.Analysis.PerchModel == "" {
				u.Analysis.PerchModel = res.model
			}
			return nil
		}); err != nil {
			return err
		}
	}
	log.Info("analysis: finished Perch on file", "detections", res.detections, "dur", time.Since(began).Round(time.Second))
	return nil
}

// run is one model's pass over a file: copy it out of storage, run the model,
// merge what it heard, cut a clip of each, and store the clips and the
// detections. The caller records the result on the file.
//
// What is wrong with the file itself comes back as unreadable; settle sorts
// that from everything else.
//
// threads is Perch's TensorFlow threads (birdnet.Options.Threads); 0 for
// BirdNET, or for TensorFlow's own default.
func (q *Queue) run(ctx context.Context, card db.Upload, f db.AudioFile, s step, threads int) (res result, err error) {
	task := q.Perf.Begin(s.model)
	var size int64
	if task != nil {
		ctx = birdnet.WithObserver(ctx, task)
		wait := q.waited(f, s)
		defer func() {
			rec := q.taskRecord(card, f, wait, size, res, err)
			rec.Threads = threads
			task.End(rec)
		}()
	}
	task.Phase("download")
	local, cleanup, err := q.fetch(ctx, f)
	defer cleanup()
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return result{}, unreadable("the audio isn't in storage")
	case err != nil:
		return result{}, fmt.Errorf("copying the audio out of storage: %w", err)
	}
	if task != nil {
		if fi, err := os.Stat(local); err == nil {
			size = fi.Size()
		}
	}

	night, _ := time.ParseInLocation(time.DateOnly, f.Night, pacific)
	opts := settings
	opts.Model, opts.Threads = s.model, threads
	if lat, lon := card.Recorder.Latitude, card.Recorder.Longitude; lat != 0 || lon != 0 {
		opts.Location = &birdnet.Location{Latitude: lat, Longitude: lon, Week: birdnet.Week(night)}
	}
	task.Phase("analyze")
	ran, err := q.analyzer.Analyze(ctx, []string{local}, opts)
	switch {
	case err != nil:
		return result{}, err
	case len(ran.Files) != 1:
		return result{}, fmt.Errorf("BirdNET reported %d files for one", len(ran.Files))
	case ran.Files[0].Error != "":
		return result{}, unreadable(ran.Files[0].Error)
	}

	res = result{model: ran.Model}
	res.start, res.startKnown = RecordedAt(f.Path)
	if f.RecordedAt != nil {
		res.start, res.startKnown = *f.RecordedAt, true
	}
	start := res.start
	if !res.startKnown {
		// Without a time in the name, the night is all that is known.
		start = night
	}
	found := merge(ran.Files[0].Detections)
	detections := make([]db.Detection, len(found))
	clips := make([]birdnet.Clip, len(found))
	for i, d := range found {
		detections[i] = db.Detection{
			// Set here rather than by the store, because the clip is named by it.
			ID:          db.ModelDetectionID(s.model, f.ID, int64(d.StartSec*1000), d.ScientificName),
			Model:       s.model,
			AudioFileID: f.ID, RecorderID: card.RecorderID,
			DetectedAt: start.Add(time.Duration(d.StartSec * float64(time.Second))).UTC().Truncate(time.Millisecond),
			Night:      f.Night, StartSec: d.StartSec, EndSec: d.EndSec,
			ScientificName: d.ScientificName, CommonName: d.CommonName, Confidence: d.Confidence,
			ReviewStatus: db.ReviewUnreviewed,
		}
		clipStart, clipEnd := clipSpan(d)
		clips[i] = birdnet.Clip{Path: filepath.Join(filepath.Dir(local), fmt.Sprintf("clip-%d.flac", i)), StartSec: clipStart, EndSec: clipEnd}
	}
	// Cut even a file with nothing heard in it, for its duration and sample rate.
	task.Phase("clip")
	res.recording, err = q.analyzer.Cut(ctx, local, clips)
	if err != nil {
		return result{}, fmt.Errorf("cutting clips: %w", err)
	}
	// A model and cutting clips take minutes over a file, long enough for its
	// card to be deleted. Checking before anything is stored keeps a deleted
	// card's clips out of storage.
	task.Phase("store")
	if _, err := q.store.GetAudioFile(ctx, card.ID, f.ID); err != nil {
		return result{}, err
	}
	for i, c := range res.recording.Clips {
		name := storage.ClipName(card.ID, detections[i].ID)
		if err := q.putFile(ctx, name, c.Path); err != nil {
			return result{}, fmt.Errorf("storing a clip: %w", err)
		}
		detections[i].Clip = &db.Clip{BlobName: name, StartSec: c.StartSec, EndSec: c.EndSec}
	}
	if len(detections) > 0 {
		if err := q.store.UpsertDetections(ctx, card.ID, detections); err != nil {
			return result{}, fmt.Errorf("storing the detections: %w", err)
		}
	}
	res.detections = len(detections)
	return res, nil
}

// waited is how long a file has been waiting for a step: since it landed, for
// BirdNET, and since BirdNET finished with it, for Perch. 0 when that isn't
// recorded.
func (q *Queue) waited(f db.AudioFile, s step) float64 {
	since := f.UploadedAt
	if s.model == birdnet.ModelPerch {
		since = f.AnalyzedAt
	}
	if since == nil {
		return 0
	}
	return q.now().Sub(*since).Round(time.Second).Seconds()
}

// taskRecord is what run knows about a pass over a file, for internal/perf,
// which adds the timings, CPU and memory.
func (q *Queue) taskRecord(card db.Upload, f db.AudioFile, wait float64, size int64, res result, err error) perf.Task {
	rec := perf.Task{
		Card: card.ID, File: f.ID, Path: f.Path,
		Workers: max(settings.Workers, birdnet.DefaultWorkers), Threads: settings.Threads,
		Result: "ok", Detections: res.detections, AudioSec: res.recording.DurationSec,
		Bytes: size, WaitSec: wait,
	}
	if rec.AudioSec == 0 {
		// A pass that failed before cutting clips never learned the
		// duration; an earlier pass over the file may have.
		rec.AudioSec = f.DurationSec
	}
	var bad unreadable
	switch {
	case errors.As(err, &bad):
		rec.Result, rec.Error = "unreadable", err.Error()
	case err != nil:
		rec.Result, rec.Error = "error", err.Error()
	}
	return rec
}

var errSkip = errors.New("analysis: file is no longer queued")

// fetch copies a file's audio to a temporary file BirdNET can read, and
// returns its path and a func that removes it. The copy keeps the extension
// the file had on the card, because BirdNET picks a decoder by it.
func (q *Queue) fetch(ctx context.Context, f db.AudioFile) (string, func(), error) {
	noop := func() {}
	if f.BlobName == "" {
		return "", noop, storage.ErrNotFound
	}
	dir, err := os.MkdirTemp("", "birdsense-analysis-")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	src, err := q.files.Open(ctx, f.BlobName)
	if err != nil {
		return "", cleanup, err
	}
	defer src.Close()
	local := filepath.Join(dir, f.ID+strings.ToLower(path.Ext(f.Path)))
	dst, err := os.Create(local)
	if err != nil {
		return "", cleanup, err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return "", cleanup, err
	}
	return local, cleanup, dst.Close()
}

// putFile copies a file from local disk into file storage.
func (q *Queue) putFile(ctx context.Context, name, local string) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	return q.files.Put(ctx, name, src)
}

// settle turns a step that failed on a file into what the queue does next.
// What is wrong with the file itself is recorded on it at once. A card deleted
// from under the run is not the file's failure: its documents are gone, so
// drain finishes the delete instead. Anything else -- the model crashing or
// being killed, or storing what it heard failing -- goes through retryOrFail,
// because otherwise a failure that never passes (a blob 403 after a role
// change, a store that keeps rejecting the write) re-runs the whole
// multi-minute pass over a ~300 MB file for good, and every card behind it
// waits.
//
// held is the context the step ran under (hold): a run cut short because the
// claim went to someone else is left to them, and a run stopped because the
// queue is stopping lets go of the file.
func (q *Queue) settle(ctx, held context.Context, s step, f db.AudioFile, err error) error {
	var bad unreadable
	switch {
	case errors.As(err, &bad):
		return q.fail(ctx, s, f, string(bad))
	case ctx.Err() != nil:
		q.release(ctx, f, s)
		return ctx.Err()
	case errors.Is(context.Cause(held), errLostClaim):
		q.log.Warn("analysis: lost the claim on a file part way through; leaving it to whoever has it", "upload", f.UploadID, "path", f.Path, "model", s.model)
		return nil
	case errors.Is(err, db.ErrNotFound):
		return err
	}
	return q.retryOrFail(ctx, s, f, err)
}

// retryOrFail puts a file back in the step's queue and pauses the queue,
// unless the file has had its tries, in which case the step fails.
// The count is on the file (db.Claim.Attempts), so it survives the worker.
func (q *Queue) retryOrFail(ctx context.Context, s step, f db.AudioFile, cause error) error {
	why := fmt.Sprintf("analysis failed on this file %d times: %s", maxAttempts, firstLine(cause.Error()))
	at := q.stamp()
	attempt, gaveUp := 0, false
	_, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		if !s.waiting(*f) {
			return errSkip
		}
		c := s.claim(f)
		c.Attempts++
		attempt = c.Attempts
		if attempt >= maxAttempts {
			gaveUp = true
			s.failed(f, why, at)
			return nil
		}
		s.requeue(f)
		// 30 s after the first failure, 60 s after the second: long enough
		// for a blip to pass, and it spaces this file's tries without holding
		// up anyone else's.
		after := q.now().UTC().Add(min(q.retryDelay<<(attempt-1), maxRetryDelay))
		s.claim(f).RetryAfter = &after
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return nil
	case err != nil:
		return err
	case gaveUp:
		q.log.Error("analysis: giving up on a file", "upload", f.UploadID, "path", f.Path, "model", s.model, "err", cause)
		return nil
	}
	return fmt.Errorf("%w on %s (%s, attempt %d of %d): %w", errRetry, f.Path, s.model, attempt, maxAttempts, cause)
}

// fail records that a step can't be done on a file, and why.
func (q *Queue) fail(ctx context.Context, s step, f db.AudioFile, why string) error {
	q.log.Warn("analysis: file failed", "upload", f.UploadID, "path", f.Path, "model", s.model, "why", why)
	at := q.stamp()
	_, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		s.failed(f, why, at)
		return nil
	})
	return err
}

// tally recounts a card's analysis from its files, and finishes a card that
// has nothing left queued for BirdNET or Perch: in_review, or needs_attention
// when BirdNET couldn't analyze some files. Perch failing on a file doesn't
// need a coordinator's attention -- BirdNET's result is still there -- so it
// is only shown on the file.
func (q *Queue) tally(ctx context.Context, id string) error {
	q.tallyMu.Lock()
	defer q.tallyMu.Unlock()
	files, err := q.store.ListAudioFiles(ctx, id)
	if err != nil {
		return err
	}
	var analyzed, failed, detections, perchDetections, waiting int
	for _, f := range files {
		switch {
		case f.StatusDetail == db.AudioDetailNotOnCard:
		case f.Status == db.AudioAnalyzed:
			analyzed++
			detections += f.DetectionCount
		case f.Status == db.AudioFailed:
			failed++
		default:
			waiting++
		}
		if f.Perch != nil && f.StatusDetail != db.AudioDetailNotOnCard {
			perchDetections += f.Perch.DetectionCount
			// A file waiting for Perch holds the card in processing, and so
			// holds on to its audio: retention only sweeps finished cards.
			if q.Perch && perchWaiting(f) {
				waiting++
			}
		}
	}
	finished := q.stamp()
	_, err = q.store.UpdateUpload(ctx, id, func(u *db.Upload) error {
		if u.Status != db.StatusProcessing {
			return nil
		}
		finishing := waiting == 0 && analyzed+failed > 0
		if !finishing && u.FilesAnalyzed == analyzed && u.FilesFailed == failed &&
			u.DetectionCount == detections && u.PerchDetectionCount == perchDetections {
			// Nothing to write: a card is looked at on every scan, and
			// replacing it unchanged is a write for nothing.
			return errSkip
		}
		u.FilesAnalyzed, u.FilesFailed, u.DetectionCount = analyzed, failed, detections
		u.PerchDetectionCount = perchDetections
		// A card with no files on record has nothing to finish on.
		if !finishing {
			return nil
		}
		u.ProcessedAt = &finished
		if u.Analysis != nil {
			u.Analysis.FinishedAt = &finished
		}
		u.Status, u.StatusDetail = db.StatusInReview, ""
		if failed > 0 {
			u.Status, u.StatusDetail = db.StatusNeedsAttention, plural(failed, "file")+" not analyzed"
		}
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

func (q *Queue) stamp() time.Time {
	return q.now().UTC().Truncate(time.Second)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	const most = 200
	if len(s) > most {
		s = s[:most] + "…"
	}
	return s
}

// sleep waits d, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pacific is where the recorders are. Their clocks, and so the times in file
// names, are local.
var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// namedStart matches the start of a recording in its file name, as recorders
// write it: "Marymoor_20260723_160624(-0700).wav" began at 16:06:24 on July 23,
// local time, at UTC-7. The trailing boundary is what keeps a longer run of
// digits from matching. frontend/js/card-scan.js reads the same timestamp to
// put a file on its night.
var namedStart = regexp.MustCompile(`(?:^|\D)(\d{8}_\d{6})(?:\D|$)`)

// namedOffset matches the UTC offset that may follow that time, in
// parentheses. It is matched on its own rather than as a second group of
// namedStart: RE2 has no lookahead, so namedStart's trailing boundary consumes
// the "(" that opens the offset, and one pattern for both silently never
// matches the offset of the very format it documents.
var namedOffset = regexp.MustCompile(`\(([+-]\d{4})\)`)

// RecordedAt is when a recording started, from its file name. The UTC offset
// in the name is used when there is one; otherwise the time is Pacific.
func RecordedAt(cardPath string) (time.Time, bool) {
	name := path.Base(cardPath)
	m := namedStart.FindStringSubmatchIndex(name)
	if m == nil {
		return time.Time{}, false
	}
	stamp := name[m[2]:m[3]]
	loc := pacific
	// Search from the end of the time itself, not the end of the match, which
	// may have taken the offset's opening parenthesis with it.
	if o := namedOffset.FindStringSubmatch(name[m[3]:]); o != nil {
		offset, err := time.Parse("-0700", o[1])
		if err != nil {
			return time.Time{}, false
		}
		_, secs := offset.Zone()
		loc = time.FixedZone(o[1], secs)
	}
	t, err := time.ParseInLocation("20060102_150405", stamp, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
