// Package analysis runs BirdNET over the audio of received cards and stores
// what it hears.
//
// The queue is the database. A card whose last file has landed is
// "processing", and each of its files in "uploaded" status is waiting for
// BirdNET. Queue.Run works through them one file at a time, oldest card first:
// it copies the file out of storage, runs internal/birdnet over it, merges
// the consecutive windows in which one species was heard (merge.go), cuts a
// clip of each detection into storage, writes the detections, and marks the
// file analyzed.
// When no file on a card is left, the card moves on to in_review, or to
// needs_attention if BirdNET couldn't read some of it.
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
	// retryDelay is the wait after such a failure, times the attempt number.
	retryDelay = 30 * time.Second
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

	wake chan struct{}
	// attempts counts BirdNET failures per audio file id. It is only touched
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
}

// New returns a queue that reads audio from files and writes to store.
func New(store db.Store, files storage.Store, analyzer Analyzer, log *slog.Logger) *Queue {
	q := &Queue{
		store: store, files: files, analyzer: analyzer, log: log,
		wake:     make(chan struct{}, 1),
		attempts: map[string]int{},
		now:      time.Now, retryDelay: retryDelay, checkDelay: checkDelay,
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
func (q *Queue) Run(ctx context.Context) {
	if !q.waitReady(ctx) {
		return
	}
	for {
		err := q.drain(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			q.setStatus(StateFailing, err.Error())
			q.log.Error("analysis: pausing after a failure", "err", err, "retry_in", q.retryDelay)
			if !sleep(ctx, q.retryDelay) {
				return
			}
			continue
		}
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
		err := q.analyzer.Check(checkCtx)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			q.setStatus(StateReady, "")
			q.log.Info("analysis: BirdNET is available; working through received cards")
			return true
		}
		q.setStatus(StateUnavailable, err.Error())
		q.log.Warn("BirdNET isn't available, so received cards will wait in processing; set BIRDSENSE_BIRDNET_PYTHON and BIRDSENSE_BIRDNET_SCRIPT",
			"err", err, "retry_in", delay)
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
func (q *Queue) drain(ctx context.Context) error {
	// Wake-ups that arrive while draining are covered by this pass, as long as
	// the card list is read after them.
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
		err := q.processCard(ctx, card)
		if errors.Is(err, db.ErrNotFound) {
			// Only deleting a card removes its documents, so one that has gone
			// from under the queue is a card being deleted. Detections stored
			// for it after the delete swept past them go now.
			q.log.Info("analysis: card deleted while being analyzed", "upload", card.ID)
			if err := q.store.DeleteUpload(ctx, card.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func receivedAt(u db.Upload) time.Time {
	if u.ReceivedAt != nil {
		return *u.ReceivedAt
	}
	return u.UpdatedAt
}

func (q *Queue) processCard(ctx context.Context, card db.Upload) error {
	files, err := q.store.ListAudioFiles(ctx, card.ID)
	if err != nil {
		return err
	}
	for _, f := range files {
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

// queued reports whether a file is waiting for BirdNET. A file found
// "analyzing" was cut off by a restart, so it waits again.
func queued(f db.AudioFile) bool {
	return f.Status == db.AudioUploaded || f.Status == db.AudioAnalyzing
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

// analyzeFile runs BirdNET over one file and stores the result. It returns an
// error only for something that should pause the queue: the store failing, or
// an attempt that failed in a way that may pass. What is wrong with the file
// itself is recorded on the file, and an attempt that keeps failing runs out
// of tries (storingFailed, retryOrFail) rather than repeating for good.
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

	local, cleanup, err := q.fetch(ctx, f)
	defer cleanup()
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return q.fail(ctx, f, "the audio isn't in storage")
	case err != nil:
		return q.retryOrFail(ctx, f, fmt.Errorf("copying the audio out of storage: %w", err))
	}

	night, _ := time.ParseInLocation(time.DateOnly, f.Night, pacific)
	opts := settings
	if lat, lon := card.Recorder.Latitude, card.Recorder.Longitude; lat != 0 || lon != 0 {
		opts.Location = &birdnet.Location{Latitude: lat, Longitude: lon, Week: birdnet.Week(night)}
	}
	res, err := q.analyzer.Analyze(ctx, []string{local}, opts)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return q.retryOrFail(ctx, f, err)
	case len(res.Files) != 1:
		return q.retryOrFail(ctx, f, fmt.Errorf("BirdNET reported %d files for one", len(res.Files)))
	case res.Files[0].Error != "":
		return q.fail(ctx, f, res.Files[0].Error)
	}

	start, startKnown := RecordedAt(f.Path)
	if f.RecordedAt != nil {
		start, startKnown = *f.RecordedAt, true
	}
	if !startKnown {
		// Without a time in the name, the night is all that is known.
		start = night
	}
	found := merge(res.Files[0].Detections)
	detections := make([]db.Detection, len(found))
	clips := make([]birdnet.Clip, len(found))
	for i, d := range found {
		detections[i] = db.Detection{
			// Set here rather than by the store, because the clip is named by it.
			ID:          db.DetectionID(f.ID, int64(d.StartSec*1000), d.ScientificName),
			AudioFileID: f.ID, RecorderID: card.RecorderID,
			DetectedAt: start.Add(time.Duration(d.StartSec * float64(time.Second))).UTC().Truncate(time.Millisecond),
			Night:      f.Night, StartSec: d.StartSec, EndSec: d.EndSec,
			ScientificName: d.ScientificName, CommonName: d.CommonName, Confidence: d.Confidence,
			ReviewStatus: db.ReviewUnreviewed,
		}
		clipStart, clipEnd := clipSpan(d)
		clips[i] = birdnet.Clip{Path: filepath.Join(filepath.Dir(local), fmt.Sprintf("clip-%d.wav", i)), StartSec: clipStart, EndSec: clipEnd}
	}
	// Cut even a file with nothing heard in it, for its duration and sample rate.
	recording, err := q.analyzer.Cut(ctx, local, clips)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		return q.retryOrFail(ctx, f, fmt.Errorf("cutting clips: %w", err))
	}
	// BirdNET and cutting clips take minutes over a file, long enough for its
	// card to be deleted. Checking before anything is stored keeps a deleted
	// card's clips out of storage.
	if _, err := q.store.GetAudioFile(ctx, card.ID, f.ID); err != nil {
		return q.storingFailed(ctx, f, err)
	}
	for i, c := range recording.Clips {
		name := storage.ClipName(card.ID, detections[i].ID)
		if err := q.putFile(ctx, name, c.Path); err != nil {
			return q.storingFailed(ctx, f, fmt.Errorf("storing a clip: %w", err))
		}
		detections[i].Clip = &db.Clip{BlobName: name, StartSec: c.StartSec, EndSec: c.EndSec}
	}
	if len(detections) > 0 {
		if err := q.store.UpsertDetections(ctx, card.ID, detections); err != nil {
			return q.storingFailed(ctx, f, fmt.Errorf("storing the detections: %w", err))
		}
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Status, f.StatusDetail = db.AudioAnalyzed, ""
		f.AnalyzedAt, f.DetectionCount = &analyzed, len(detections)
		f.DurationSec, f.SampleRate = recording.DurationSec, recording.SampleRate
		if f.RecordedAt == nil && startKnown {
			t := start.UTC()
			f.RecordedAt = &t
		}
		return nil
	}); err != nil {
		return q.storingFailed(ctx, f, fmt.Errorf("marking the file analyzed: %w", err))
	}
	if res.Model != "" && card.Analysis != nil && card.Analysis.Model == "" {
		if _, err := q.store.UpdateUpload(ctx, card.ID, func(u *db.Upload) error {
			if u.Analysis != nil && u.Analysis.Model == "" {
				u.Analysis.Model = res.Model
			}
			return nil
		}); err != nil {
			return err
		}
	}
	delete(q.attempts, f.ID)
	log.Info("analysis: finished file", "detections", len(detections), "dur", time.Since(began).Round(time.Second))
	return nil
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

// storingFailed is a failure in the steps after BirdNET has read the file:
// storing a clip, storing the detections, or recording the result. They go
// through the same attempt counter as a crashed analyzer, because otherwise a
// failure that never passes -- a blob 403 after a role change, a store that
// keeps rejecting the write -- re-runs the whole multi-minute pass over a
// ~300 MB file for good, and every card behind it waits.
//
// A card deleted from under the run is not the file's failure: its documents
// are gone, so drain finishes the delete instead.
func (q *Queue) storingFailed(ctx context.Context, f db.AudioFile, cause error) error {
	if ctx.Err() != nil || errors.Is(cause, db.ErrNotFound) {
		return cause
	}
	return q.retryOrFail(ctx, f, cause)
}

// retryOrFail puts a file back in the queue and pauses the queue, unless the
// file has had its tries, in which case it fails.
func (q *Queue) retryOrFail(ctx context.Context, f db.AudioFile, cause error) error {
	q.attempts[f.ID]++
	if q.attempts[f.ID] >= maxAttempts {
		delete(q.attempts, f.ID)
		q.log.Error("analysis: giving up on a file", "upload", f.UploadID, "path", f.Path, "err", cause)
		return q.fail(ctx, f, "analysis failed on this file "+fmt.Sprint(maxAttempts)+" times: "+firstLine(cause.Error()))
	}
	if _, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		if f.Status == db.AudioAnalyzing {
			f.Status = db.AudioUploaded
		}
		return nil
	}); err != nil {
		return err
	}
	return fmt.Errorf("%w on %s (attempt %d of %d): %w", errRetry, f.Path, q.attempts[f.ID], maxAttempts, cause)
}

// fail records that a file can't be analyzed, and why.
func (q *Queue) fail(ctx context.Context, f db.AudioFile, why string) error {
	q.log.Warn("analysis: file failed", "upload", f.UploadID, "path", f.Path, "why", why)
	analyzed := q.stamp()
	_, err := q.store.UpdateAudioFile(ctx, f.UploadID, f.ID, func(f *db.AudioFile) error {
		f.Status, f.StatusDetail, f.AnalyzedAt, f.DetectionCount = db.AudioFailed, why, &analyzed, 0
		return nil
	})
	return err
}

// tally recounts a card's analysis from its files, and finishes a card that
// has nothing left queued: in_review, or needs_attention when some files
// couldn't be analyzed.
func (q *Queue) tally(ctx context.Context, id string) error {
	files, err := q.store.ListAudioFiles(ctx, id)
	if err != nil {
		return err
	}
	var analyzed, failed, detections, waiting int
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
	}
	finished := q.stamp()
	_, err = q.store.UpdateUpload(ctx, id, func(u *db.Upload) error {
		if u.Status != db.StatusProcessing {
			return nil
		}
		u.FilesAnalyzed, u.FilesFailed, u.DetectionCount = analyzed, failed, detections
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
