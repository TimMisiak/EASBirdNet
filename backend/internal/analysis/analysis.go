// Package analysis runs BirdNET over the audio of received cards and stores
// what it hears, and, when Queue.Perch is set, runs Google's Perch v2 over
// the same files as a second opinion.
//
// The queue is the database. A card whose last file has landed is
// "processing", and each of its files in "uploaded" status is waiting for
// BirdNET. Queue.Run works through them one file at a time, oldest card first:
// it copies the file out of storage, runs internal/birdnet over it, merges
// the consecutive windows in which one species was heard (merge.go), cuts a
// clip of each detection into storage, writes the detections, and marks the
// file analyzed.
// With Perch on, a file BirdNET has analyzed is then queued for Perch, which
// goes through the same steps and stores its detections beside BirdNET's,
// each marked with the model that heard it. It is a step of its own
// (db.AudioFile.Perch), taken one file at a time after BirdNET has caught up,
// so a card waiting for BirdNET never waits behind Perch, and Perch failing on
// a file leaves BirdNET's result on it as it was.
// When no file on a card is left for either, the card moves on to in_review,
// or to needs_attention if BirdNET couldn't read some of it.
//
// Because the state lives in the Store rather than in memory, a restart (a
// deploy, a crash, a replica scaled in) loses nothing: the next Run picks up
// the files still waiting, and a file that was mid-analysis is run again.
// Detection ids are derived from what was heard, so running a file twice
// overwrites its detections rather than duplicating them.
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

	wake chan struct{}
	// attempts counts failures per step and audio file id (step.attemptKey).
	// It is only touched
	// by Run's goroutine, and a restart forgetting it just allows a few more
	// tries.
	attempts map[string]int

	// mu guards status, which Run writes and Status reads from whatever
	// goroutine an HTTP handler is on.
	mu     sync.Mutex
	status Status

	// Clock and waits, so tests don't sleep.
	now        func() time.Time
	retryDelay time.Duration
	checkDelay time.Duration
	// pause is sleep, so a test can see how long Run waited without waiting.
	pause func(context.Context, time.Duration) bool
}

// New returns a queue that reads audio from files and writes to store.
func New(store db.Store, files storage.Store, analyzer Analyzer, log *slog.Logger) *Queue {
	q := &Queue{
		store: store, files: files, analyzer: analyzer, log: log,
		wake:     make(chan struct{}, 1),
		attempts: map[string]int{},
		now:      time.Now, retryDelay: retryDelay, checkDelay: checkDelay,
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
		if err != nil {
			q.setStatus(StateFailing, err.Error())
			q.log.Error("analysis: pausing after a failure", "err", err, "retry_in", delay)
			if !q.pause(ctx, delay) {
				return
			}
			delay = min(2*delay, maxRetryDelay)
			continue
		}
		delay = q.retryDelay
		q.setStatus(StateReady, "")
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
	}
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

// errRetry is an attempt on a file that failed and is worth trying again after
// a pause.
var errRetry = errors.New("analysis: attempt failed")

// drain analyzes every queued file on every processing card, and finishes each
// card as its last file is done. It returns early on a failure Run should
// pause for.
//
// BirdNET comes first, on every card, and Perch (when it is on) takes one file
// at a time after that, going back to BirdNET between each: Perch is several
// times slower, so a card that arrives while a long one is getting its second
// opinion still gets its first one straight away.
func (q *Queue) drain(ctx context.Context) error {
	for {
		// Wake-ups that arrive while draining are covered by this pass, as
		// long as the card list is read after them.
		select {
		case <-q.wake:
		default:
		}
		cards, err := q.store.ListUploads(ctx, db.UploadFilter{Status: db.StatusProcessing})
		if err != nil {
			return err
		}
		// First come, first analyzed.
		slices.SortStableFunc(cards, func(a, b db.Upload) int { return receivedAt(a).Compare(receivedAt(b)) })
		for _, card := range cards {
			if err := q.gone(ctx, card, q.processCard(ctx, card)); err != nil {
				return err
			}
		}
		if !q.Perch {
			return nil
		}
		ran := false
		for _, card := range cards {
			ran, err = q.perchNext(ctx, card)
			if err := q.gone(ctx, card, err); err != nil {
				return err
			}
			if ran {
				break
			}
		}
		if !ran {
			return nil
		}
	}
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

// processCard runs BirdNET over each of a card's queued files, and finishes
// the card if that leaves nothing waiting.
func (q *Queue) processCard(ctx context.Context, card db.Upload) error {
	files, err := q.store.ListAudioFiles(ctx, card.ID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if !q.Perch && perchWaiting(f) {
			// Queued for Perch while it was on, and it is off now. The file
			// never got its second opinion, so it says none.
			if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
				if perchWaiting(*f) {
					f.Perch = nil
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if !queued(f) {
			continue
		}
		if card.Analysis == nil {
			if card, err = q.startCard(ctx, card.ID); err != nil {
				return err
			}
		}
		if err := q.analyzeFile(ctx, card, f); err != nil {
			return err
		}
		if err := q.tally(ctx, card.ID); err != nil {
			return err
		}
	}
	// A card with nothing left queued is finished, including one whose last
	// file was done before a restart got to finishing the card.
	return q.tally(ctx, card.ID)
}

// perchNext runs Perch over the first of a card's files waiting for it, and
// reports whether there was one.
func (q *Queue) perchNext(ctx context.Context, card db.Upload) (bool, error) {
	files, err := q.store.ListAudioFiles(ctx, card.ID)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if !perchWaiting(f) || queued(f) {
			continue
		}
		if card.Analysis == nil {
			if card, err = q.startCard(ctx, card.ID); err != nil {
				return false, err
			}
		}
		if err := q.perchFile(ctx, card, f); err != nil {
			return true, err
		}
		return true, q.tally(ctx, card.ID)
	}
	return false, nil
}

// queued reports whether a file is waiting for BirdNET. A file found
// "analyzing" was cut off by a restart, so it waits again.
func queued(f db.AudioFile) bool {
	return f.Status == db.AudioUploaded || f.Status == db.AudioAnalyzing
}

// perchWaiting reports whether a file is waiting for Perch, the same way.
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
	// requeue puts a file this step was cut off on back in its queue.
	requeue func(f *db.AudioFile)
	// failed records that this step can't be done on the file, and why.
	failed func(f *db.AudioFile, why string, at time.Time)
}

var birdnetStep = step{
	model: birdnet.ModelBirdNET,
	requeue: func(f *db.AudioFile) {
		if f.Status == db.AudioAnalyzing {
			f.Status = db.AudioUploaded
		}
	},
	failed: func(f *db.AudioFile, why string, at time.Time) {
		f.Status, f.StatusDetail, f.AnalyzedAt, f.DetectionCount = db.AudioFailed, why, &at, 0
	},
}

var perchStep = step{
	model: birdnet.ModelPerch,
	requeue: func(f *db.AudioFile) {
		if f.Perch != nil && f.Perch.Status == db.PerchAnalyzing {
			f.Perch.Status = db.PerchQueued
		}
	},
	failed: func(f *db.AudioFile, why string, at time.Time) {
		f.Perch = &db.PerchRun{Status: db.PerchFailed, StatusDetail: why, AnalyzedAt: &at}
	},
}

// attemptKey is what a step's failures on a file are counted under.
func (s step) attemptKey(f db.AudioFile) string {
	return s.model + ":" + f.ID
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
	f, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		if !queued(*f) {
			return errSkip
		}
		f.Status, f.StatusDetail = db.AudioAnalyzing, ""
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return nil
	case err != nil:
		return err
	}
	log := q.log.With("upload", card.ID, "path", f.Path)
	log.Info("analysis: starting file")
	began := time.Now()

	res, err := q.run(ctx, card, f, birdnetStep)
	if err != nil {
		return q.settle(ctx, birdnetStep, f, err)
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Status, f.StatusDetail = db.AudioAnalyzed, ""
		f.AnalyzedAt, f.DetectionCount = &analyzed, res.detections
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
		return q.settle(ctx, birdnetStep, f, fmt.Errorf("marking the file analyzed: %w", err))
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
	delete(q.attempts, birdnetStep.attemptKey(f))
	log.Info("analysis: finished file", "detections", res.detections, "dur", time.Since(began).Round(time.Second))
	return nil
}

// perchFile runs Perch over one file BirdNET has analyzed, and stores what it
// heard beside BirdNET's detections. It returns an error the same way
// analyzeFile does, and records what goes wrong on the file's Perch step.
func (q *Queue) perchFile(ctx context.Context, card db.Upload, f db.AudioFile) error {
	f, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		if !perchWaiting(*f) {
			return errSkip
		}
		f.Perch.Status, f.Perch.StatusDetail = db.PerchAnalyzing, ""
		return nil
	})
	switch {
	case errors.Is(err, errSkip):
		return nil
	case err != nil:
		return err
	}
	log := q.log.With("upload", card.ID, "path", f.Path)
	log.Info("analysis: starting Perch on file")
	began := time.Now()

	res, err := q.run(ctx, card, f, perchStep)
	if err != nil {
		return q.settle(ctx, perchStep, f, err)
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Perch = &db.PerchRun{Status: db.PerchAnalyzed, AnalyzedAt: &analyzed, DetectionCount: res.detections}
		return nil
	}); err != nil {
		return q.settle(ctx, perchStep, f, fmt.Errorf("marking the file analyzed by Perch: %w", err))
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
	delete(q.attempts, perchStep.attemptKey(f))
	log.Info("analysis: finished Perch on file", "detections", res.detections, "dur", time.Since(began).Round(time.Second))
	return nil
}

// run is one model's pass over a file: copy it out of storage, run the model,
// merge what it heard, cut a clip of each, and store the clips and the
// detections. The caller records the result on the file.
//
// What is wrong with the file itself comes back as unreadable; settle sorts
// that from everything else.
func (q *Queue) run(ctx context.Context, card db.Upload, f db.AudioFile, s step) (result, error) {
	local, cleanup, err := q.fetch(ctx, f)
	defer cleanup()
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return result{}, unreadable("the audio isn't in storage")
	case err != nil:
		return result{}, fmt.Errorf("copying the audio out of storage: %w", err)
	}

	night, _ := time.ParseInLocation(time.DateOnly, f.Night, pacific)
	opts := settings
	opts.Model = s.model
	if lat, lon := card.Recorder.Latitude, card.Recorder.Longitude; lat != 0 || lon != 0 {
		opts.Location = &birdnet.Location{Latitude: lat, Longitude: lon, Week: birdnet.Week(night)}
	}
	ran, err := q.analyzer.Analyze(ctx, []string{local}, opts)
	switch {
	case err != nil:
		return result{}, err
	case len(ran.Files) != 1:
		return result{}, fmt.Errorf("BirdNET reported %d files for one", len(ran.Files))
	case ran.Files[0].Error != "":
		return result{}, unreadable(ran.Files[0].Error)
	}

	res := result{model: ran.Model}
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
	res.recording, err = q.analyzer.Cut(ctx, local, clips)
	if err != nil {
		return result{}, fmt.Errorf("cutting clips: %w", err)
	}
	// A model and cutting clips take minutes over a file, long enough for its
	// card to be deleted. Checking before anything is stored keeps a deleted
	// card's clips out of storage.
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
func (q *Queue) settle(ctx context.Context, s step, f db.AudioFile, err error) error {
	var bad unreadable
	switch {
	case errors.As(err, &bad):
		return q.fail(ctx, s, f, string(bad))
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, db.ErrNotFound):
		return err
	}
	return q.retryOrFail(ctx, s, f, err)
}

// retryOrFail puts a file back in the step's queue and pauses the queue,
// unless the file has had its tries, in which case the step fails.
func (q *Queue) retryOrFail(ctx context.Context, s step, f db.AudioFile, cause error) error {
	key := s.attemptKey(f)
	q.attempts[key]++
	if q.attempts[key] >= maxAttempts {
		delete(q.attempts, key)
		q.log.Error("analysis: giving up on a file", "upload", f.UploadID, "path", f.Path, "model", s.model, "err", cause)
		return q.fail(ctx, s, f, "analysis failed on this file "+fmt.Sprint(maxAttempts)+" times: "+firstLine(cause.Error()))
	}
	if _, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		s.requeue(f)
		return nil
	}); err != nil {
		return err
	}
	return fmt.Errorf("%w on %s (%s, attempt %d of %d): %w", errRetry, f.Path, s.model, q.attempts[key], maxAttempts, cause)
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
		u.FilesAnalyzed, u.FilesFailed, u.DetectionCount = analyzed, failed, detections
		u.PerchDetectionCount = perchDetections
		// A card with no files on record has nothing to finish on.
		if waiting > 0 || analyzed+failed == 0 {
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
