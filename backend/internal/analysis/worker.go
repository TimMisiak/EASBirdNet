package analysis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// The queue run as a job (ANALYSIS.md, *The job*): a worker replica claims
// files until there are none, then exits, and the web app starts replicas
// when there is work. Run is the same queue kept up for good in the web app's
// own process, which is what development and a deployment without the job
// use.

// maxFailedPasses is how many passes in a row a worker lets fail as a whole
// -- the store unreachable -- before it gives up and exits with an error. The
// web app starts another when the store is back and the work still waits.
const maxFailedPasses = 5

// ErrUnavailable is a worker that can't run the models at all.
var ErrUnavailable = errors.New("analysis: the models can't run here")

// RunUntilIdle works through the queue like Run, and returns once nothing has
// been claimable for idle: nil then, or when ctx ends (a stopped worker lets
// go of its files). It returns ErrUnavailable, at once, if the models can't
// run, and an error if passes keep failing as a whole.
//
// A file waiting on its retryAfter within idle is waited for. One held by
// another worker, or waiting longer, is left: the worker exits, and the web
// app starts another if the work is still there.
func (q *Queue) RunUntilIdle(ctx context.Context, idle time.Duration) error {
	check := q.analyzer.Check
	if q.Perch {
		check = q.analyzer.CheckPerch
	}
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	err := check(checkCtx)
	cancel()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		q.setStatus(StateUnavailable, err.Error())
		// The same words Run logs, which is what the alert matches on.
		q.log.Error("BirdNET isn't available, so this worker can't analyze anything", "err", err, "perch", q.Perch)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	q.setStatus(StateReady, "")

	lastWork := time.Now()
	delay, failed := q.retryDelay, 0
	for {
		q.started = 0
		err := q.drain(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err != nil && !errors.Is(err, errRetry):
			failed++
			q.setStatus(StateFailing, err.Error())
			if failed >= maxFailedPasses {
				return fmt.Errorf("analysis: %d passes in a row failed: %w", failed, err)
			}
			q.log.Error("analysis: pausing after a failure", "err", err, "retry_in", delay)
			if !q.pause(ctx, delay) {
				return nil
			}
			delay = min(2*delay, maxRetryDelay)
			continue
		case err != nil:
			q.setStatus(StateFailing, err.Error())
		default:
			q.setStatus(StateReady, "")
		}
		failed, delay = 0, q.retryDelay
		if q.started > 0 {
			lastWork = time.Now()
			continue
		}
		wait := idle - time.Since(lastWork)
		if wait <= 0 {
			q.log.Info("analysis: nothing left to claim; this worker is done")
			return nil
		}
		if !q.nextWake.IsZero() {
			wait = min(wait, max(q.nextWake.Sub(q.now()), 0)+time.Second)
		}
		if !q.pause(ctx, wait) {
			return nil
		}
	}
}

// Pending counts the steps still to be done on processing cards -- waiting,
// or being run by someone -- which is what the web app sizes the job by. It
// only reads.
func (q *Queue) Pending(ctx context.Context) (int, error) {
	cards, err := q.store.ListUploads(ctx, db.UploadFilter{Status: db.StatusProcessing})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, card := range cards {
		files, err := q.store.ListAudioFiles(ctx, card.ID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, f := range files {
			for _, s := range q.steps() {
				if s.waiting(f) {
					n++
				}
			}
		}
	}
	return n, nil
}

// PublishStatus returns an OnStatus that writes the status into file storage
// (storage.AnalysisStatusName), where the web app reads it back (ReadStatus):
// a worker's status is otherwise in a process no screen can see. With several
// workers the last to report wins, which is what a coordinator wants to know
// about -- the most recent thing that happened.
func PublishStatus(files storage.Store, log interface{ Warn(string, ...any) }) func(Status) {
	var last []byte
	return func(s Status) {
		// Only when it changes: a status is set after every pass.
		key, _ := json.Marshal(struct{ State, Detail string }{s.State, s.Detail})
		if bytes.Equal(key, last) {
			return
		}
		b, err := json.Marshal(s)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := files.Put(ctx, storage.AnalysisStatusName, bytes.NewReader(b)); err != nil {
			log.Warn("analysis: publishing the queue's status", "err", err)
			return
		}
		last = key
	}
}

// ReadStatus is the status a worker last published, and false if none has.
func ReadStatus(ctx context.Context, files storage.Store) (Status, bool, error) {
	r, err := files.Open(ctx, storage.AnalysisStatusName)
	if errors.Is(err, storage.ErrNotFound) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	defer r.Close()
	var s Status
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err == nil, err
}
