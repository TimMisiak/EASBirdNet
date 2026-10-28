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

// The review API (REVIEW.md, M3): the cards with bouts, a card's bouts, and
// a reviewer's verdict on one. A bout settles when its verdicts agree, and
// its verdict then goes to every detection in it. Verdicts are blind: no
// one sees a bout's other verdicts until they have given their own, except a
// birding expert, who sees them always.

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

// giveVerdict records the caller's verdict on a bout, in place of any they
// gave before: {verdict, scientificName?, commonName?}, the species only
// for a confirm of one other than the bout's. A bout its verdicts settle
// has the verdict copied to its detections before this answers {bout}.
// Giving the same verdict again is harmless, and finishes that copy if it
// was cut short.
func (h *handlers) giveVerdict(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		Verdict        string `json:"verdict"`
		ScientificName string `json:"scientificName"`
		CommonName     string `json:"commonName"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	body.ScientificName, body.CommonName = strings.TrimSpace(body.ScientificName), strings.TrimSpace(body.CommonName)
	switch {
	case body.Verdict != db.VerdictConfirm && body.Verdict != db.VerdictReject && body.Verdict != db.VerdictUnsure:
		h.problem(w, http.StatusBadRequest, "verdict must be confirm, reject or unsure")
		return
	case body.Verdict != db.VerdictConfirm && (body.ScientificName != "" || body.CommonName != ""):
		h.problem(w, http.StatusBadRequest, "only a confirm names a species")
		return
	case body.ScientificName == "" && body.CommonName != "":
		h.problem(w, http.StatusBadRequest, "name the species by its scientific name")
		return
	}
	card, ok := h.reviewCard(w, r)
	if !ok {
		return
	}
	v := db.BoutVerdict{
		UserID: me.ID, UserName: me.Name, Verdict: body.Verdict,
		ScientificName: body.ScientificName, CommonName: body.CommonName, At: h.stamp(),
	}
	var was string
	b, err := h.store.UpdateBout(r.Context(), card.ID, r.PathValue("id"), func(b *db.Bout) error {
		was = b.Status
		return review.Give(b, v)
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
