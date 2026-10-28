package review

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

const (
	// backstop is how often the preparer looks with nothing to do: for a
	// card BirdNET finished in the analysis job, which can't wake it.
	backstop = 5 * time.Minute
	// firstRetry and maxRetry bound the wait after a pass that failed, which
	// is usually the store being unreachable.
	firstRetry = 30 * time.Second
	maxRetry   = 10 * time.Minute
)

// Preparer readies cards for review, in the web app: once BirdNET has been
// over every file of a card, and again whenever the review rules change, it
// routes the card's detections by the rules and rebuilds the card's summary
// (REVIEW.md, Preparing a card).
//
// The worker already routes each file's detections as it stores them, so for
// a card analyzed under today's rules a pass changes nothing and only
// rebuilds the summary, which is what puts right an adjustment a build
// missed. A card analyzed before the rules, or before they last changed, is
// routed again here. Like deletion and analysis, the work is in the
// documents: a card whose review.rulesVersion isn't the rules' is work, and a
// pass that stops part way is finished by the next.
type Preparer struct {
	store db.Store
	log   *slog.Logger
	now   func() time.Time

	wake     chan struct{}
	failures int
}

// NewPreparer returns a preparer over store.
func NewPreparer(store db.Store, log *slog.Logger) *Preparer {
	return &Preparer{store: store, log: log, now: time.Now, wake: make(chan struct{}, 1)}
}

// Wake says the rules have changed or a card is ready. It never blocks.
func (p *Preparer) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run prepares at once, then whenever woken, after a failure's delay, and on
// the backstop, until ctx ends.
func (p *Preparer) Run(ctx context.Context) {
	for {
		again := p.Pass(ctx)
		if again <= 0 {
			again = backstop
		}
		t := time.NewTimer(again)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// Pass prepares every card that needs it, one at a time. It answers how soon
// to try again after a failure, or zero when nothing failed.
func (p *Preparer) Pass(ctx context.Context) time.Duration {
	rules, err := p.store.GetReviewRules(ctx)
	if errors.Is(err, db.ErrNotFound) {
		// No rules, nothing to route by.
		p.failures = 0
		return 0
	}
	if err != nil {
		p.log.Error("preparing cards for review: reading the rules", "err", err)
		return p.retry()
	}
	table := NewTable(&rules)
	cards, err := p.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		p.log.Error("preparing cards for review: listing them", "err", err)
		return p.retry()
	}
	failed := false
	for _, u := range cards {
		if ctx.Err() != nil {
			return 0
		}
		if !Ready(u) || (u.Review != nil && u.Review.RulesVersion == rules.Version) {
			continue
		}
		if err := p.Prepare(ctx, table, u.ID); err != nil && ctx.Err() == nil {
			failed = true
			p.log.Error("preparing a card for review", "upload", u.ID, "err", err)
		}
	}
	if failed {
		return p.retry()
	}
	p.failures = 0
	return 0
}

// Ready says whether a card is ready for review: BirdNET has been over every
// file on it (Perch may still be running), and it is neither still arriving,
// nor being deleted, nor closed.
func Ready(u db.Upload) bool {
	switch u.Status {
	case db.StatusProcessing, db.StatusInReview, db.StatusNeedsAttention:
		return u.FileCount > 0 && u.FilesAnalyzed+u.FilesFailed >= u.FileCount
	}
	return false
}

// Prepare routes one card's BirdNET detections by table and rebuilds its
// summary, then records the rules' version on the card. A detection a person
// has reviewed keeps their verdict and only has its route brought up to date.
func (p *Preparer) Prepare(ctx context.Context, table *Table, ref string) error {
	started := p.now()
	var changed []string
	err := p.store.EachDetection(ctx, db.DetectionFilter{UploadID: ref, Model: db.ModelBirdNET}, func(d db.Detection) error {
		if table.Apply(&d) {
			changed = append(changed, d.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Each is read again as it is written, so a review given since the read
	// above keeps its verdict.
	if len(changed) > 0 {
		if err := p.store.UpdateDetections(ctx, ref, changed, func(d *db.Detection) error {
			table.Apply(d)
			return nil
		}); err != nil {
			return err
		}
	}
	// Routing a card for the first time can take minutes. A card deleted
	// meanwhile is left to the deleter: a summary built now could land after
	// it had passed, naming a card that is gone.
	if u, err := p.store.GetUpload(ctx, ref); errors.Is(err, db.ErrNotFound) || (err == nil && u.Status == db.StatusDeleting) {
		return nil
	} else if err != nil {
		return err
	}
	summary, err := Build(ctx, p.store, ref)
	if err != nil {
		return err
	}
	at := p.now()
	_, err = p.store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		u.Review = &db.UploadReview{RulesVersion: table.Version(), PreparedAt: at}
		return nil
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	p.log.Info("prepared a card for review", "upload", ref, "rules", table.Version(),
		"routed", len(changed), "counted", summary.Detections, "took", at.Sub(started).Round(time.Millisecond))
	return nil
}

// retry is how long to wait after a failed pass: 30 s, doubling to 10
// minutes while passes keep failing.
func (p *Preparer) retry() time.Duration {
	wait := firstRetry << min(p.failures, 5)
	p.failures++
	return min(wait, maxRetry)
}
