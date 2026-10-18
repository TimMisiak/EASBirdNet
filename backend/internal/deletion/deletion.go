// Package deletion removes the cards a coordinator has deleted.
//
// Deleting a card is its audio and clips in storage, tens of thousands of
// documents in Cosmos, and the card itself -- more than one request can be
// trusted to finish. Serverless Cosmos throttles a large card's delete as a
// matter of course, the ingress ends a request after 240 s, and a deploy
// gives a request in flight about 30. So the request only marks the card
// (Mark), and a Deleter in the web app does the rest.
//
// Like the analysis queue and retention, the work is in the database: a card
// in deleting is work, and a pass that stops part way is finished by the
// next. The steps go in the order a delete that stops part way can be run
// again from -- storage first, then the card's files and detections, then the
// card -- so nothing is ever left that no document names.
//
// A card is left alone while analysis is still running a step of one of its
// files. Marking it stops any new step being claimed (the queue only takes
// files of cards in processing), but one already running goes on to write its
// clips and detections, and clips written after the sweep passed would be in
// storage with nothing naming them. So the sweep waits out the claim.
package deletion

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

const (
	// backstop is how often the deleter looks with nothing to do: for a card
	// marked while it wasn't listening, or by a server that stopped first.
	backstop = 5 * time.Minute
	// settle is how long a card is left after it is marked before the sweep
	// starts on it. The analysis queue lists processing cards and then claims
	// their files; a claim of a file on a card listed just before the mark
	// lands just after it, and must be there to be waited for.
	settle = time.Minute
	// firstRetry and maxRetry bound the wait after a pass that failed. A
	// failure is usually the store or storage being unreachable, which a
	// retry every few seconds would only add to.
	firstRetry = 30 * time.Second
	maxRetry   = 10 * time.Minute
)

// DetailRetrying is a deleting card's statusDetail after a pass failed on it.
const DetailRetrying = "couldn't finish deleting; trying again"

// Mark moves a card to deleting, as of at. A card already deleting is left as
// it was, so marking it again changes nothing. It is ErrNotFound if there is
// no such card.
func Mark(ctx context.Context, store db.Store, ref string, at time.Time) (db.Upload, error) {
	return store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		if u.Status != db.StatusDeleting {
			u.Status, u.StatusDetail, u.DeleteRequestedAt = db.StatusDeleting, "", &at
		}
		return nil
	})
}

// Deleter removes cards in deleting. One runs in the web app.
type Deleter struct {
	store db.Store
	files storage.Store
	log   *slog.Logger
	now   func() time.Time

	// Settle is how long after a card is marked the sweep starts on it.
	// Tests set it to zero.
	Settle time.Duration

	wake chan struct{}
	// failures is how many passes in a row have failed on some card, which
	// is what the wait before the next grows with.
	failures int
}

// New returns a deleter over store and files.
func New(store db.Store, files storage.Store, log *slog.Logger) *Deleter {
	return &Deleter{
		store: store, files: files, log: log, now: time.Now,
		Settle: settle, wake: make(chan struct{}, 1),
	}
}

// Wake says a card has been marked. It never blocks.
func (d *Deleter) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run sweeps at once, then whenever woken, when a card it left is due another
// look, and on the backstop, until ctx ends.
func (d *Deleter) Run(ctx context.Context) {
	for {
		again := d.Sweep(ctx)
		if again <= 0 {
			again = backstop
		}
		t := time.NewTimer(again)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-d.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Sweep deletes every card in deleting that it can, one at a time, oldest
// mark first. It answers how soon a card it had to leave is worth another
// look, or zero when it left none.
func (d *Deleter) Sweep(ctx context.Context) time.Duration {
	cards, err := d.store.ListUploads(ctx, db.UploadFilter{Status: db.StatusDeleting})
	if err != nil {
		d.log.Error("deleting cards: listing them", "err", err)
		return d.retry()
	}
	slices.SortFunc(cards, func(a, b db.Upload) int {
		return cmp.Compare(markedAt(a).UnixNano(), markedAt(b).UnixNano())
	})
	var again time.Duration
	failed := false
	for _, u := range cards {
		if ctx.Err() != nil {
			return 0
		}
		wait, err := d.deleteCard(ctx, u)
		switch {
		case err != nil && ctx.Err() == nil:
			failed = true
			d.log.Error("deleting a card", "upload", u.ID, "err", err)
			d.noteFailure(ctx, u)
		case wait > 0:
			again = soonest(again, wait)
		}
	}
	if failed {
		return soonest(again, d.retry())
	}
	d.failures = 0
	return again
}

// deleteCard deletes one card, unless it is too soon or analysis still holds
// one of its files, when it answers how long to leave it.
func (d *Deleter) deleteCard(ctx context.Context, u db.Upload) (wait time.Duration, err error) {
	now := d.now()
	if due := markedAt(u).Add(d.Settle); now.Before(due) {
		return due.Sub(now), nil
	}
	files, err := d.store.ListAudioFiles(ctx, u.ID)
	if err != nil {
		return 0, err
	}
	if until, held := claimed(files, now); held {
		d.log.Info("deleting a card: waiting for analysis to let go of its files", "upload", u.ID, "until", until)
		return until.Sub(now), nil
	}
	if err := d.deleteAudio(ctx, u); err != nil {
		return 0, err
	}
	// Not found means someone else -- the analysis queue, finding the card
	// going -- got to the end of it first.
	if err := d.store.DeleteUpload(ctx, u.ID); err != nil && !errors.Is(err, db.ErrNotFound) {
		return 0, err
	}
	d.log.Info("deleted a card", "upload", u.ID, "files", len(files), "took", d.now().Sub(now).Round(time.Millisecond))
	return 0, nil
}

// deleteAudio removes everything storage holds for a card: its files, finished
// or not, and its clips, which is everything under its prefix. The prefix can
// spell two references the same, so if another card shares it the audio is
// left where it is, rather than taking that card's with it.
func (d *Deleter) deleteAudio(ctx context.Context, u db.Upload) error {
	prefix := storage.Segment(u.ID)
	all, err := d.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID != u.ID && storage.Segment(other.ID) == prefix {
			d.log.Warn("deleting a card: leaving its audio and clips in storage, another card shares its prefix",
				"upload", u.ID, "other", other.ID, "prefix", storage.Name(prefix))
			return nil
		}
	}
	return d.files.DeleteAll(ctx, prefix)
}

// noteFailure puts DetailRetrying on a card a pass failed on, so the card
// lists say it is stuck rather than just deleting.
func (d *Deleter) noteFailure(ctx context.Context, u db.Upload) {
	if u.StatusDetail == DetailRetrying {
		return
	}
	_, err := d.store.UpdateUpload(ctx, u.ID, func(u *db.Upload) error {
		if u.Status == db.StatusDeleting {
			u.StatusDetail = DetailRetrying
		}
		return nil
	})
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		d.log.Warn("deleting a card: noting that it failed", "upload", u.ID, "err", err)
	}
}

// retry is the wait after a failed pass: firstRetry, doubling each pass in a
// row that fails, up to maxRetry.
func (d *Deleter) retry() time.Duration {
	wait := min(firstRetry<<min(d.failures, 10), maxRetry)
	d.failures++
	return wait
}

// claimed reports whether analysis is running a step of any of files -- one
// in analyzing, BirdNET's or Perch's, whose lease is still in force -- and
// when the first such lease runs out. A step in analyzing whose lease has
// lapsed, or that has none, isn't being run by anyone.
func claimed(files []db.AudioFile, now time.Time) (until time.Time, held bool) {
	hold := func(running bool, c db.Claim) {
		if running && c.LeaseUntil != nil && now.Before(*c.LeaseUntil) {
			if !held || c.LeaseUntil.Before(until) {
				until = *c.LeaseUntil
			}
			held = true
		}
	}
	for _, f := range files {
		hold(f.Status == db.AudioAnalyzing, f.Claim)
		if f.Perch != nil {
			hold(f.Perch.Status == db.PerchAnalyzing, f.Perch.Claim)
		}
	}
	return until, held
}

// markedAt is when a card was marked. A card in deleting without the time,
// which only a hand-edited document would be, is due at once.
func markedAt(u db.Upload) time.Time {
	if u.DeleteRequestedAt != nil {
		return *u.DeleteRequestedAt
	}
	return time.Time{}
}

// soonest is the sooner of two waits, where zero is no wait at all.
func soonest(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	return min(a, b)
}
