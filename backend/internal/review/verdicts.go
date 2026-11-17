package review

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
)

// maxListeners is how many people hear a bout: three verdicts that settle
// nothing make it disputed, for a birding expert.
const maxListeners = 3

// What a verdict can meet that keeps it from being given.
var (
	ErrSettled  = errors.New("this bout is settled already")
	ErrDisputed = errors.New("this bout's listeners didn't agree; it waits for a birding expert")
)

// Give records v as its giver's verdict on b, in place of any they gave
// before, and settles b by its verdicts (Settle). A verdict the same as the
// one its giver has given changes nothing, on a settled bout too, so giving
// it twice is giving it once; any other is refused once b is settled
// (ErrSettled) or disputed (ErrDisputed).
func Give(b *db.Bout, v db.BoutVerdict) error {
	v = ownSpecies(*b, v)
	i := slices.IndexFunc(b.Reviews, func(r db.BoutVerdict) bool { return r.UserID == v.UserID })
	if i >= 0 && b.Reviews[i].Verdict == v.Verdict && b.Reviews[i].ScientificName == v.ScientificName {
		return nil
	}
	switch b.Status {
	case db.BoutConfirmed, db.BoutRejected:
		return ErrSettled
	case db.BoutDisputed:
		return ErrDisputed
	}
	if i >= 0 {
		b.Reviews[i] = v
	} else {
		b.Reviews = append(b.Reviews, v)
	}
	Settle(b)
	return nil
}

// ErrNotAFinalSay is a final say that is neither a confirm nor a reject.
var ErrNotAFinalSay = errors.New("a final say confirms or rejects")

// Decide records a birding expert's final say on b (db.Bout.Resolution): a
// confirm or reject that settles it whatever its listeners said -- a
// disputed bout's, or an override of one they settled, or of one still
// open. A later final say replaces it; the same one again changes nothing.
func Decide(b *db.Bout, v db.BoutVerdict) error {
	if v.Verdict != db.VerdictConfirm && v.Verdict != db.VerdictReject {
		return ErrNotAFinalSay
	}
	v = ownSpecies(*b, v)
	if r := b.Resolution; r != nil && r.Verdict == v.Verdict && r.ScientificName == v.ScientificName {
		return nil
	}
	b.Resolution = &v
	Settle(b)
	return nil
}

// ownSpecies is v with no species named when it names none or the bout's
// own, which is what a confirm of the bout's species stores.
func ownSpecies(b db.Bout, v db.BoutVerdict) db.BoutVerdict {
	if v.Verdict != db.VerdictConfirm || v.ScientificName == b.ScientificName {
		v.ScientificName, v.CommonName = "", ""
	}
	return v
}

// matches says whether two verdicts agree: both reject, or both confirm the
// same species. Unsure agrees with nothing.
func matches(a, b db.BoutVerdict) bool {
	switch {
	case a.Verdict != b.Verdict:
		return false
	case a.Verdict == db.VerdictReject:
		return true
	case a.Verdict == db.VerdictConfirm:
		return a.ScientificName == b.ScientificName
	}
	return false
}

// Settle sets b's status by its verdicts (REVIEW.md, How a bout resolves):
// settled as Settled says, otherwise disputed once maxListeners have given
// one, otherwise open.
func Settle(b *db.Bout) {
	if s, ok := Settled(*b); ok {
		b.Status = s.BoutStatus()
		return
	}
	b.Status = db.BoutOpen
	if len(b.Reviews) >= maxListeners {
		b.Status = db.BoutDisputed
	}
}

// Settlement is what settled a bout: the verdict, and everyone who gave it.
type Settlement struct {
	// Verdict is the last of them to be given, which settled it.
	Verdict db.BoutVerdict
	By      []db.BoutVerdict
}

// BoutStatus is the bout status s gives.
func (s Settlement) BoutStatus() string {
	if s.Verdict.Verdict == db.VerdictReject {
		return db.BoutRejected
	}
	return db.BoutConfirmed
}

// Settled is what settles b, if anything does: a birding expert's
// resolution; on a spot-check, one confirm of its own species with nothing
// else given; otherwise two verdicts that match.
func Settled(b db.Bout) (Settlement, bool) {
	if r := b.Resolution; r != nil && (r.Verdict == db.VerdictConfirm || r.Verdict == db.VerdictReject) {
		return Settlement{Verdict: *r, By: []db.BoutVerdict{*r}}, true
	}
	own := func(v db.BoutVerdict) bool { return v.Verdict == db.VerdictConfirm && v.ScientificName == "" }
	if b.Kind == db.BoutSpotCheck && len(b.Reviews) > 0 && !slices.ContainsFunc(b.Reviews, func(v db.BoutVerdict) bool { return !own(v) }) {
		return settlement(b.Reviews), true
	}
	for i, v := range b.Reviews {
		var by []db.BoutVerdict
		for _, w := range b.Reviews[i:] {
			if matches(v, w) {
				by = append(by, w)
			}
		}
		if len(by) >= 2 {
			return settlement(by), true
		}
	}
	return Settlement{}, false
}

func settlement(by []db.BoutVerdict) Settlement {
	last := slices.MaxFunc(by, func(a, b db.BoutVerdict) int { return a.At.Compare(b.At) })
	return Settlement{Verdict: last, By: slices.Clone(by)}
}

// Carry gives d the verdict that settled its bout: confirmed or rejected,
// and a review naming the bout, who settled it, everyone who agreed, and
// the species, when they confirmed another. A detection a person reviewed
// on its own keeps their verdict. It says whether d changed.
func Carry(d *db.Detection, b db.Bout, s Settlement) bool {
	if d.Review != nil && d.Review.BoutID != b.ID {
		return false
	}
	status := db.ReviewConfirmed
	if s.Verdict.Verdict == db.VerdictReject {
		status = db.ReviewRejected
	}
	r := &db.Review{UserID: s.Verdict.UserID, UserName: s.Verdict.UserName, At: s.Verdict.At, BoutID: b.ID}
	for _, v := range s.By {
		r.AgreedBy = append(r.AgreedBy, v.UserName)
	}
	if status == db.ReviewConfirmed && s.Verdict.ScientificName != "" {
		r.CorrectedScientificName, r.CorrectedCommonName = s.Verdict.ScientificName, cmp.Or(s.Verdict.CommonName, s.Verdict.ScientificName)
	}
	if d.ReviewStatus == status && d.BoutID == b.ID && sameReview(d.Review, r) {
		return false
	}
	d.ReviewStatus, d.Review, d.BoutID = status, r, b.ID
	return true
}

func sameReview(a, b *db.Review) bool {
	return a != nil && b != nil && a.UserID == b.UserID && a.UserName == b.UserName && a.At.Equal(b.At) &&
		a.BoutID == b.BoutID && slices.Equal(a.AgreedBy, b.AgreedBy) &&
		a.CorrectedScientificName == b.CorrectedScientificName && a.CorrectedCommonName == b.CorrectedCommonName
}

// Resolve does what a verdict owes the rest of its bout's card, given the
// bout as it was written and the status it had before: a settled bout's
// verdict goes to its detections and their card's summary (Copy), the
// card's review counts move with the bout's status, and a species confirmed
// that the rules have no row for is added to them (AddConfirmed). Each part
// can be done again, so a verdict given twice finishes what the first one
// didn't.
func Resolve(ctx context.Context, store db.Store, was string, b db.Bout) error {
	s, settled := Settled(b)
	if settled {
		if err := Copy(ctx, store, b, s); err != nil {
			return err
		}
	}
	if was != b.Status {
		_, err := store.UpdateUpload(ctx, b.UploadID, func(u *db.Upload) error {
			if u.Review != nil {
				count(u.Review, was, -1)
				count(u.Review, b.Status, 1)
			}
			return nil
		})
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return err
		}
	}
	if settled && s.BoutStatus() == db.BoutConfirmed {
		scientific, common := boutSettled{b, s}.confirmed()
		if _, err := AddConfirmed(ctx, store, scientific, common); err != nil {
			return err
		}
	}
	return nil
}

// confirmed is the species a settled bout was confirmed as: its own, or
// the one its listeners named instead.
func (b boutSettled) confirmed() (scientificName, commonName string) {
	if v := b.Settlement.Verdict; v.ScientificName != "" {
		return v.ScientificName, cmp.Or(v.CommonName, v.ScientificName)
	}
	return b.ScientificName, b.CommonName
}

// count moves a card's review counts by n bouts of status.
func count(r *db.UploadReview, status string, n int) {
	switch status {
	case db.BoutConfirmed, db.BoutRejected:
		r.Resolved = max(r.Resolved+n, 0)
	case db.BoutDisputed:
		r.Disputed = max(r.Disputed+n, 0)
	}
}

// Copy gives a settled bout's verdict to every detection in it that doesn't
// carry it yet (Carry), and adjusts the card's summary by what changed. A
// copy that fails part way leaves the summary for the card's next build.
func Copy(ctx context.Context, store db.Store, b db.Bout, s Settlement) error {
	var mu sync.Mutex
	before := map[string]db.Detection{}
	err := store.UpdateDetections(ctx, b.UploadID, b.DetectionIDs, func(d *db.Detection) error {
		was := *d
		if !Carry(d, b, s) {
			return db.ErrUnchanged
		}
		mu.Lock()
		before[d.ID] = was
		mu.Unlock()
		return nil
	})
	if err != nil || len(before) == 0 {
		return err
	}
	var was, now []db.Detection
	for _, d := range before {
		was = append(was, d)
		Carry(&d, b, s)
		now = append(now, d)
	}
	return Adjust(ctx, store, b.UploadID, was, now)
}

// AddConfirmed adds a species reviewers confirmed to the rules when they
// have no row for it, to be reviewed above the default floor, so the rules
// page lists it from then on (REVIEW.md). It says whether it added one. The
// row is a version of the rules of its own, which a save from the page
// made before it keeps (db.ReviewRules.SavedVersion).
func AddConfirmed(ctx context.Context, store db.Store, scientificName, commonName string) (bool, error) {
	for range 3 {
		rules, err := store.GetReviewRules(ctx)
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// Rows are kept in name order (api.saveReviewRules).
		i, found := slices.BinarySearchFunc(rules.Species, scientificName, func(r db.SpeciesRule, name string) int {
			return strings.Compare(r.ScientificName, name)
		})
		if found {
			return false, nil
		}
		rules.SavedVersion = rules.Saved()
		rules.Species = slices.Insert(rules.Species, i, db.SpeciesRule{
			ScientificName: scientificName, CommonName: commonName,
			RuleAction: db.RuleAction{Action: db.RouteReview, MinConfidence: rules.Defaults.MinConfidence},
			Source:     db.RuleSourceReview, AddedIn: rules.Version + 1,
		})
		_, err = store.SaveReviewRules(ctx, rules)
		if errors.Is(err, db.ErrConflict) {
			continue // saved meanwhile: look again
		}
		return err == nil, err
	}
	return false, db.ErrConflict
}

// Unprepare marks a card to be prepared again, which finishes what a
// verdict's Resolve didn't: the preparer copies a settled bout's verdict
// to any of its detections that don't carry it, rebuilds the summary and
// counts the bouts again.
func Unprepare(ctx context.Context, store db.Store, ref string) error {
	_, err := store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		if u.Review != nil {
			u.Review.Preparation = 0
		}
		return nil
	})
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	return err
}
