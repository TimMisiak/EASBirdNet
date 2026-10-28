package review

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// Preparation is the preparation Prepare gives a card
// (db.UploadReview.Preparation): one with an older one is prepared again,
// whatever rules it was prepared by. 2 is routing and bouts.
const Preparation = 2

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
// routes the card's detections by the rules, groups what people are to hear
// into bouts, and rebuilds the card's summary (REVIEW.md, Preparing a card).
//
// The worker already routes each file's detections as it stores them, so for
// a card analyzed under today's rules routing changes nothing; what a pass
// adds is the bouts, which need the whole card, and the summary's rebuild,
// which is what puts right an adjustment a build missed. A card analyzed
// before the rules, or before they last changed, is routed again here. Like deletion and analysis, the work is in the
// documents: a card whose review.rulesVersion isn't the rules', or whose
// review.preparation is older than Preparation, is work, and a pass that
// stops part way is finished by the next.
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
		if !Ready(u) || (u.Review != nil && u.Review.RulesVersion == rules.Version && u.Review.Preparation >= Preparation) {
			continue
		}
		err := p.Prepare(ctx, table, u.ID)
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.Is(err, errHeardMeanwhile):
			failed = true
			p.log.Info("preparing a card for review", "upload", u.ID, "err", err)
		default:
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

// errHeardMeanwhile is a bout someone gave a verdict on while its card was
// being prepared: what was made from the card as it was read may now
// disagree with it, so the card is prepared again.
var errHeardMeanwhile = errors.New("a bout was reviewed while its card was being prepared; preparing it again")

// Prepare readies one card for review by table (REVIEW.md, Preparing a card):
//
//  1. It routes the card's BirdNET detections, except those in a bout someone
//     has heard. A detection a person has reviewed keeps their verdict and
//     only has its route brought up to date.
//  2. It bouts what is routed review, and picks spot-checks from bouts of
//     what was accepted; writes the bouts that are new or changed, deletes
//     those no longer made, and sets boutId on the detections whose bout
//     changed. Bouts someone has heard are left as they are.
//  3. It rebuilds the card's summary, and records on the card the rules'
//     version and how many bouts it has.
//
// Every write is idempotent, so a pass cut part way is finished by the next.
func (p *Preparer) Prepare(ctx context.Context, table *Table, ref string) error {
	started := p.now()
	card, err := p.store.GetUpload(ctx, ref)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	stored, err := p.store.ListBouts(ctx, ref)
	if err != nil {
		return err
	}
	// heardIn is each detection in a bout someone has heard, and its bout.
	heardIn := map[string]string{}
	keptChecks := map[string]int{}
	for _, b := range stored {
		if !b.Heard() {
			continue
		}
		for _, id := range b.DetectionIDs {
			heardIn[id] = b.ID
		}
		if b.Kind == db.BoutSpotCheck {
			keptChecks[b.ScientificName]++
		}
	}

	// boutOf is the bout each detection is in now; rerouted, those whose
	// route or status the rules change.
	boutOf := map[string]string{}
	rerouted := map[string]bool{}
	var toReview, accepted []Member
	err = p.store.EachDetection(ctx, db.DetectionFilter{UploadID: ref, Model: db.ModelBirdNET}, func(d db.Detection) error {
		boutOf[d.ID] = d.BoutID
		if _, heard := heardIn[d.ID]; heard {
			return nil
		}
		if table.Apply(&d) {
			rerouted[d.ID] = true
		}
		if d.Review != nil {
			return nil // reviewed one at a time, before bouts: left out of them
		}
		switch {
		case d.Route == db.RouteReview:
			toReview = append(toReview, MemberOf(d))
		case d.Route == db.RouteAccept && d.ReviewStatus == db.ReviewAccepted:
			accepted = append(accepted, MemberOf(d))
		}
		return nil
	})
	if err != nil {
		return err
	}
	gap := time.Duration(table.Rules.BoutGapMinutes) * time.Minute
	made := append(Bouts(card, db.BoutReview, toReview, gap),
		SpotChecks(Bouts(card, db.BoutSpotCheck, accepted, gap), table.Rules.SpotChecksPerSpecies, keptChecks)...)

	// want is the bout each detection should be in: the one it is heard in,
	// or the one just made.
	want := maps.Clone(heardIn)
	for _, b := range made {
		for _, id := range b.DetectionIDs {
			want[id] = b.ID
		}
	}
	written, deleted, err := p.writeBouts(ctx, ref, stored, made)
	if err != nil {
		return err
	}
	var changed []string
	for id, in := range boutOf {
		if rerouted[id] || in != want[id] {
			changed = append(changed, id)
		}
	}
	// Each is read again as it is written, so a review given since the read
	// above keeps its verdict.
	if len(changed) > 0 {
		if err := p.store.UpdateDetections(ctx, ref, changed, func(d *db.Detection) error {
			if _, heard := heardIn[d.ID]; !heard {
				table.Apply(d)
			}
			d.BoutID = want[d.ID]
			return nil
		}); err != nil {
			return err
		}
	}

	// Preparing a card for the first time can take minutes. A card deleted
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
	counts := db.UploadReview{RulesVersion: table.Version(), Preparation: Preparation, PreparedAt: p.now(), Bouts: len(made)}
	for _, b := range stored {
		if !b.Heard() {
			continue
		}
		counts.Bouts++
		switch b.Status {
		case db.BoutConfirmed, db.BoutRejected:
			counts.Resolved++
		case db.BoutDisputed:
			counts.Disputed++
		}
	}
	_, err = p.store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		u.Review = &counts
		return nil
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	p.log.Info("prepared a card for review", "upload", ref, "rules", table.Version(),
		"rerouted", len(rerouted), "bouts", counts.Bouts, "boutsWritten", written, "boutsDeleted", deleted,
		"detectionsWritten", len(changed), "counted", summary.Detections,
		"took", counts.PreparedAt.Sub(started).Round(time.Millisecond))
	return nil
}

// writeBouts stores what preparing made of a card's bouts over what was
// stored: the new and the changed are written, and those no longer made are
// deleted, unless someone has heard them -- which, if it happened since they
// were read, is errHeardMeanwhile. It answers how many it wrote and deleted.
func (p *Preparer) writeBouts(ctx context.Context, ref string, stored, made []db.Bout) (written, deleted int, err error) {
	old := make(map[string]db.Bout, len(stored))
	for _, b := range stored {
		old[b.ID] = b
	}
	unheard := func(b *db.Bout) error {
		if b.Heard() {
			return errHeardMeanwhile
		}
		return nil
	}
	for _, b := range made {
		was, had := old[b.ID]
		delete(old, b.ID)
		if had && (was.Heard() || sameBout(was, b)) {
			continue
		}
		if !had {
			_, err = p.store.CreateBout(ctx, b)
			if err == nil {
				written++
				continue
			}
			if !errors.Is(err, db.ErrConflict) {
				return written, deleted, err
			}
			// Made by another pass since the read: write over it as over
			// any other.
		}
		_, err = p.store.UpdateBout(ctx, ref, b.ID, func(v *db.Bout) error {
			if err := unheard(v); err != nil {
				return err
			}
			*v = b
			return nil
		})
		if err != nil {
			return written, deleted, err
		}
		written++
	}
	for id, b := range old {
		if b.Heard() {
			continue
		}
		err := p.store.DeleteBout(ctx, ref, id, func(b db.Bout) error { return unheard(&b) })
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return written, deleted, err
		}
		deleted++
	}
	return written, deleted, nil
}

// retry is how long to wait after a failed pass: 30 s, doubling to 10
// minutes while passes keep failing.
func (p *Preparer) retry() time.Duration {
	wait := firstRetry << min(p.failures, 5)
	p.failures++
	return min(wait, maxRetry)
}
