// Package analysis runs BirdNET over the audio of received cards and stores
// what it hears.
//
// The queue is the database. A card whose last file has landed is
// "processing", and each of its files in "uploaded" status is waiting for
// BirdNET. Queue.Run works through them one file at a time, oldest card first:
// it copies the file out of storage, runs internal/birdnet over it, writes a
// detection for everything above the threshold, and marks the file analyzed.
// When no file on a card is left, the card moves on to in_review, or to
// needs_attention if BirdNET couldn't read some of it.
//
// Because the state lives in the Store rather than in memory, a restart (a
// deploy, a crash, a replica scaled in) loses nothing: the next Run picks up
// the files still waiting, and a file that was mid-analysis is run again.
// Detection ids are derived from what was heard, so running a file twice
// overwrites its detections rather than duplicating them.
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
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// Analyzer is what runs BirdNET: birdnet.Analyzer, or a fake in tests.
type Analyzer interface {
	Analyze(ctx context.Context, paths []string, opts birdnet.Options) (birdnet.Result, error)
}

// Settings every card is analyzed with. They are recorded on the card
// (db.Analysis), so a detection can be traced back to them.
var settings = birdnet.Options{
	MinConfidence: birdnet.DefaultMinConfidence,
	Sensitivity:   birdnet.DefaultSensitivity,
	OverlapSec:    0,
}

const (
	// maxAttempts is how many times a file is tried when BirdNET itself fails
	// (the process crashes or is killed), rather than reporting the file as
	// unreadable. After that the file is failed, so one bad file can't hold up
	// every card behind it.
	maxAttempts = 3
	// retryDelay is the wait after such a failure, times the attempt number.
	retryDelay = 30 * time.Second
)

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

	// Clock and retry wait, so tests don't sleep.
	now        func() time.Time
	retryDelay time.Duration
}

// New returns a queue that reads audio from files and writes to store.
func New(store db.Store, files storage.Store, analyzer Analyzer, log *slog.Logger) *Queue {
	return &Queue{
		store: store, files: files, analyzer: analyzer, log: log,
		wake:     make(chan struct{}, 1),
		attempts: map[string]int{},
		now:      time.Now, retryDelay: retryDelay,
	}
}

// Enqueue tells the queue a card has been received. It never blocks: the card
// is already queued by its status, and this only wakes Run to look.
func (q *Queue) Enqueue(reference string) {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run analyzes queued files until ctx is cancelled. It starts with whatever
// was left queued, then waits for Enqueue. Cancelling ctx kills a BirdNET run
// in flight; its file is run again next time.
func (q *Queue) Run(ctx context.Context) {
	for {
		err := q.drain(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			q.log.Error("analysis: pausing after a failure", "err", err, "retry_in", q.retryDelay)
			if !sleep(ctx, q.retryDelay) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
	}
}

// errRetry is a BirdNET run that failed and is worth trying again after a
// pause.
var errRetry = errors.New("analysis: BirdNET failed")

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
		if err := q.processCard(ctx, card); err != nil {
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
// BirdNET failing in a way that may pass. What is wrong with the file itself
// is recorded on the file.
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
	found := res.Files[0].Detections
	detections := make([]db.Detection, len(found))
	for i, d := range found {
		detections[i] = db.Detection{
			AudioFileID: f.ID, RecorderID: card.RecorderID,
			DetectedAt: start.Add(time.Duration(d.StartSec * float64(time.Second))).UTC().Truncate(time.Millisecond),
			Night:      f.Night, StartSec: d.StartSec, EndSec: d.EndSec,
			ScientificName: d.ScientificName, CommonName: d.CommonName, Confidence: d.Confidence,
			ReviewStatus: db.ReviewUnreviewed,
		}
	}
	if len(detections) > 0 {
		if err := q.store.UpsertDetections(ctx, card.ID, detections); err != nil {
			return err
		}
	}
	analyzed := q.stamp()
	if _, err := q.store.UpdateAudioFile(ctx, card.ID, f.ID, func(f *db.AudioFile) error {
		f.Status, f.StatusDetail = db.AudioAnalyzed, ""
		f.AnalyzedAt, f.DetectionCount = &analyzed, len(detections)
		if f.RecordedAt == nil && startKnown {
			t := start.UTC()
			f.RecordedAt = &t
		}
		return nil
	}); err != nil {
		return err
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

// retryOrFail puts a file back in the queue and pauses the queue, unless the
// file has had its tries, in which case it fails.
func (q *Queue) retryOrFail(ctx context.Context, f db.AudioFile, cause error) error {
	q.attempts[f.ID]++
	if q.attempts[f.ID] >= maxAttempts {
		delete(q.attempts, f.ID)
		q.log.Error("analysis: giving up on a file", "upload", f.UploadID, "path", f.Path, "err", cause)
		return q.fail(ctx, f, "BirdNET failed on this file "+fmt.Sprint(maxAttempts)+" times: "+firstLine(cause.Error()))
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
// local time, at UTC-7. The offset is optional. frontend/js/card-scan.js reads
// the same pattern to put a file on its night.
var namedStart = regexp.MustCompile(`(?:^|\D)(\d{8}_\d{6})(?:\D|$)(?:.*?\(([+-]\d{4})\))?`)

// RecordedAt is when a recording started, from its file name. The UTC offset
// in the name is used when there is one; otherwise the time is Pacific.
func RecordedAt(cardPath string) (time.Time, bool) {
	m := namedStart.FindStringSubmatch(path.Base(cardPath))
	if m == nil {
		return time.Time{}, false
	}
	loc := pacific
	if m[2] != "" {
		offset, err := time.Parse("-0700", m[2])
		if err != nil {
			return time.Time{}, false
		}
		_, secs := offset.Zone()
		loc = time.FixedZone(m[2], secs)
	}
	t, err := time.ParseInLocation("20060102_150405", m[1], loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
