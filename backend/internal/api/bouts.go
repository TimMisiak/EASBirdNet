package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/review"
)

// The review API (REVIEW.md): the cards with bouts, a card's bouts, and a
// reviewer's verdict on one. A bout settles when its verdicts agree, and its
// verdict then goes to every detection in it. Verdicts are blind: no one sees
// a bout's other verdicts until they have given their own, except a birding
// expert, who sees them always -- and who has the disputed bouts of every
// card to settle, and the final say on any bout.

// ReviewCard is a card with bouts, and how far its review has got.
type ReviewCard struct {
	Reference        string     `json:"reference"`
	StationID        string     `json:"stationId"`
	StationName      string     `json:"stationName"`
	FirstRecordingAt *time.Time `json:"firstRecordingAt,omitempty"`
	LastRecordingAt  *time.Time `json:"lastRecordingAt,omitempty"`
	// Bouts is how many it has, spot-checks included; Resolved how many are
	// settled, and Disputed how many wait for a birding expert.
	Bouts      int       `json:"bouts"`
	Resolved   int       `json:"resolved"`
	Disputed   int       `json:"disputed"`
	PreparedAt time.Time `json:"preparedAt"`
}

// ReviewBout is a bout as a reviewer gets it.
type ReviewBout struct {
	ID string `json:"id"`
	// Kind is db.BoutReview, or db.BoutSpotCheck for a sample of what the
	// rules accepted.
	Kind           string    `json:"kind"`
	ScientificName string    `json:"scientificName"`
	CommonName     string    `json:"commonName"`
	FirstHeardAt   time.Time `json:"firstHeardAt"`
	LastHeardAt    time.Time `json:"lastHeardAt"`
	// Detections is how many detections it holds, and Best the ids of those
	// a reviewer is played, most confident first.
	Detections    int      `json:"detections"`
	Best          []string `json:"best"`
	MaxConfidence float64  `json:"maxConfidence"`
	// Status is db.BoutOpen, BoutConfirmed, BoutRejected or BoutDisputed.
	Status string `json:"status"`
	// Listeners is how many have given a verdict, and Mine the caller's.
	Listeners int          `json:"listeners"`
	Mine      *BoutVerdict `json:"mine,omitempty"`
	// Verdicts is everyone's, once the caller may see them: after giving
	// their own, or always for a birding expert. Absent until then.
	Verdicts   []BoutVerdict `json:"verdicts,omitempty"`
	Resolution *BoutVerdict  `json:"resolution,omitempty"`
	// ForMe says it waits for the caller's verdict: open, and they haven't
	// given one.
	ForMe bool `json:"forMe"`
}

// BoutVerdict is one person's verdict on a bout.
type BoutVerdict struct {
	By string `json:"by"`
	// Verdict is db.VerdictConfirm, VerdictReject or VerdictUnsure.
	Verdict string `json:"verdict"`
	// ScientificName and CommonName are the species confirmed, when it
	// isn't the bout's own.
	ScientificName string    `json:"scientificName,omitempty"`
	CommonName     string    `json:"commonName,omitempty"`
	At             time.Time `json:"at"`
}

func reviewCardOf(u db.Upload) ReviewCard {
	c := ReviewCard{
		Reference: u.ID, StationID: u.RecorderID, StationName: u.Recorder.Name,
		FirstRecordingAt: u.FirstRecordingAt, LastRecordingAt: u.LastRecordingAt,
	}
	if u.Review != nil {
		c.Bouts, c.Resolved, c.Disputed, c.PreparedAt = u.Review.Bouts, u.Review.Resolved, u.Review.Disputed, u.Review.PreparedAt
	}
	return c
}

func boutVerdictOf(v db.BoutVerdict) BoutVerdict {
	return BoutVerdict{By: v.UserName, Verdict: v.Verdict, ScientificName: v.ScientificName, CommonName: v.CommonName, At: v.At}
}

// reviewBoutOf is b as me may see it.
func reviewBoutOf(b db.Bout, me db.User) ReviewBout {
	out := ReviewBout{
		ID: b.ID, Kind: b.Kind, ScientificName: b.ScientificName, CommonName: b.CommonName,
		FirstHeardAt: b.FirstHeardAt, LastHeardAt: b.LastHeardAt,
		Detections: len(b.DetectionIDs), Best: b.Best, MaxConfidence: b.MaxConfidence,
		Status: b.Status, Listeners: len(b.Reviews),
	}
	if i := slices.IndexFunc(b.Reviews, func(v db.BoutVerdict) bool { return v.UserID == me.ID }); i >= 0 {
		mine := boutVerdictOf(b.Reviews[i])
		out.Mine = &mine
	}
	if out.Mine != nil || me.Has(db.RoleBirdingExpert) {
		out.Verdicts = mapAll(b.Reviews, boutVerdictOf)
	}
	if b.Resolution != nil {
		r := boutVerdictOf(*b.Resolution)
		out.Resolution = &r
	}
	out.ForMe = b.Status == db.BoutOpen && out.Mine == nil
	return out
}

// reviewCard is a card that can be reviewed, or a problem answered: a card
// being deleted is as gone as one that is.
func (h *handlers) reviewCard(w http.ResponseWriter, r *http.Request) (db.Upload, bool) {
	u, err := h.store.GetUpload(r.Context(), r.PathValue("reference"))
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && u.Status == db.StatusDeleting):
		h.problem(w, http.StatusNotFound, "no such card")
		return db.Upload{}, false
	case err != nil:
		h.fail(w, r, err)
		return db.Upload{}, false
	}
	return u, true
}

// listReviewCards answers {cards}: every card with bouts, newest first.
func (h *handlers) listReviewCards(w http.ResponseWriter, r *http.Request, _ db.User) {
	uploads, err := h.store.ListUploads(r.Context(), db.UploadFilter{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cards := []ReviewCard{}
	for _, u := range uploads {
		if u.Review != nil && u.Review.Bouts > 0 && u.Status != db.StatusDeleting {
			cards = append(cards, reviewCardOf(u))
		}
	}
	h.json(w, http.StatusOK, map[string]any{"cards": cards})
}

// listBouts answers {card, bouts, left}: a card's bouts in the order they
// were heard, and how many of them wait for the caller.
func (h *handlers) listBouts(w http.ResponseWriter, r *http.Request, me db.User) {
	card, ok := h.reviewCard(w, r)
	if !ok {
		return
	}
	bouts, err := h.store.ListBouts(r.Context(), card.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]ReviewBout, len(bouts))
	left := 0
	for i, b := range bouts {
		out[i] = reviewBoutOf(b, me)
		if out[i].ForMe {
			left++
		}
	}
	h.json(w, http.StatusOK, map[string]any{"card": reviewCardOf(card), "bouts": out, "left": left})
}

// getBout answers {bout, detections}: one bout, and its detections in the
// order they were heard.
func (h *handlers) getBout(w http.ResponseWriter, r *http.Request, me db.User) {
	card, ok := h.reviewCard(w, r)
	if !ok {
		return
	}
	b, err := h.store.GetBout(r.Context(), card.ID, r.PathValue("id"))
	if errors.Is(err, db.ErrNotFound) {
		h.problem(w, http.StatusNotFound, msgNoSuchBout)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	detections, err := h.store.ListDetections(r.Context(), db.DetectionFilter{UploadID: card.ID, BoutID: b.ID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.json(w, http.StatusOK, map[string]any{"bout": reviewBoutOf(b, me), "detections": mapAll(detections, detectionOf)})
}

const msgNoSuchBout = "no such bout on this card"

// Disagreement is a disputed bout, and the card it is on.
type Disagreement struct {
	Card ReviewCard `json:"card"`
	Bout ReviewBout `json:"bout"`
}

// listDisagreements answers {disagreements}: every card's disputed bouts,
// in the order they were heard, for a birding expert to settle.
func (h *handlers) listDisagreements(w http.ResponseWriter, r *http.Request, me db.User) {
	bouts, err := h.store.ListBoutsByStatus(r.Context(), db.BoutDisputed)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cards := map[string]*db.Upload{}
	out := []Disagreement{}
	for _, b := range bouts {
		card, seen := cards[b.UploadID]
		if !seen {
			u, err := h.store.GetUpload(r.Context(), b.UploadID)
			switch {
			case err == nil && u.Status != db.StatusDeleting:
				card = &u
			case err != nil && !errors.Is(err, db.ErrNotFound):
				h.fail(w, r, err)
				return
			}
			cards[b.UploadID] = card
		}
		if card != nil {
			out = append(out, Disagreement{Card: reviewCardOf(*card), Bout: reviewBoutOf(b, me)})
		}
	}
	h.json(w, http.StatusOK, map[string]any{"disagreements": out})
}

// resolveBout records a birding expert's final say on a bout:
// {verdict, scientificName?, commonName?}, confirm or reject. It settles a
// disputed bout, or overrides how its listeners settled it -- or settles one
// still open -- and its verdict goes to the bout's detections in place of
// theirs, as a listeners' verdict does. Answers {bout}.
func (h *handlers) resolveBout(w http.ResponseWriter, r *http.Request, me db.User) {
	v, ok := h.boutVerdict(w, r, me)
	if !ok {
		return
	}
	if v.Verdict != db.VerdictConfirm && v.Verdict != db.VerdictReject {
		h.problem(w, http.StatusBadRequest, "a final say confirms or rejects")
		return
	}
	h.settleBout(w, r, me, v, review.Decide)
}

// giveVerdict records the caller's verdict on a bout, in place of any they
// gave before: {verdict, scientificName?, commonName?}, the species only
// for a confirm of one other than the bout's. A bout its verdicts settle
// has the verdict copied to its detections before this answers {bout}.
// Giving the same verdict again is harmless, and finishes that copy if it
// was cut short.
func (h *handlers) giveVerdict(w http.ResponseWriter, r *http.Request, me db.User) {
	v, ok := h.boutVerdict(w, r, me)
	if !ok {
		return
	}
	h.settleBout(w, r, me, v, review.Give)
}

// boutVerdict reads a verdict on a bout from the request, or answers a
// problem: {verdict, scientificName?, commonName?}.
func (h *handlers) boutVerdict(w http.ResponseWriter, r *http.Request, me db.User) (db.BoutVerdict, bool) {
	var body struct {
		Verdict        string `json:"verdict"`
		ScientificName string `json:"scientificName"`
		CommonName     string `json:"commonName"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return db.BoutVerdict{}, false
	}
	body.ScientificName, body.CommonName = strings.TrimSpace(body.ScientificName), strings.TrimSpace(body.CommonName)
	problem := ""
	switch {
	case body.Verdict != db.VerdictConfirm && body.Verdict != db.VerdictReject && body.Verdict != db.VerdictUnsure:
		problem = "verdict must be confirm, reject or unsure"
	case body.Verdict != db.VerdictConfirm && (body.ScientificName != "" || body.CommonName != ""):
		problem = "only a confirm names a species"
	case body.ScientificName == "" && body.CommonName != "":
		problem = "name the species by its scientific name"
	}
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return db.BoutVerdict{}, false
	}
	return db.BoutVerdict{
		UserID: me.ID, UserName: me.Name, Verdict: body.Verdict,
		ScientificName: body.ScientificName, CommonName: body.CommonName, At: h.stamp(),
	}, true
}

// settleBout records v on the bout the request names by record (review.Give
// or review.Decide), then carries out whatever that settled
// (review.Resolve), and answers {bout}.
func (h *handlers) settleBout(w http.ResponseWriter, r *http.Request, me db.User, v db.BoutVerdict, record func(*db.Bout, db.BoutVerdict) error) {
	card, ok := h.reviewCard(w, r)
	if !ok {
		return
	}
	var was string
	b, err := h.store.UpdateBout(r.Context(), card.ID, r.PathValue("id"), func(b *db.Bout) error {
		was = b.Status
		return record(b, v)
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchBout)
		return
	case errors.Is(err, review.ErrSettled), errors.Is(err, review.ErrDisputed):
		h.problem(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if was != b.Status {
		h.log.Info("bout heard", "card", card.ID, "bout", b.ID, "by", me.ID, "verdict", v.Verdict, "status", b.Status)
	}
	if err := review.Resolve(r.Context(), h.store, was, b); err != nil {
		// The verdict is stored. What is left of carrying it out, the
		// preparer finishes; failing to ask it to is the caller's to retry.
		if err := review.Unprepare(r.Context(), h.store, card.ID); err != nil {
			h.fail(w, r, err)
			return
		}
		h.log.Warn("a bout's verdict is left for the preparer to finish", "card", card.ID, "bout", b.ID, "err", err)
		if h.preparer != nil {
			h.preparer.Wake()
		}
	}
	h.json(w, http.StatusOK, map[string]any{"bout": reviewBoutOf(b, me)})
}
