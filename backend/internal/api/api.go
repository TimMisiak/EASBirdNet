// Package api holds the JSON HTTP API. Everything it serves lives under
// /api/v1/ so the static frontend can own every other path.
//
// Handlers read and write through db.Store and answer in the shapes in
// shapes.go. Sign-in is still a placeholder: it trusts the client (see
// sessionCookie).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// sessionCookie carries the signed-in person's email.
//
// PLACEHOLDER AUTH: there is no identity provider wired up and nothing signs
// this cookie, so anyone can claim any roster address. Replace the whole
// session block with real OIDC against Google and Microsoft before this is
// reachable from outside a demo.
const sessionCookie = "bs_session"

// Register mounts the API routes on mux. files is where card audio goes, and
// queue is told when a card has all its files, so BirdNET can start on it. dev
// turns on the development-only routes -- today, the roster the sign-in page
// lets you pick an account from.
func Register(mux *http.ServeMux, store db.Store, files storage.Store, queue Queue, log *slog.Logger, dev bool) {
	register(mux, &handlers{store: store, files: files, queue: queue, log: log, dev: dev, now: time.Now})
}

// Queue is the analysis queue (internal/analysis). A received card is queued
// by its status already; Enqueue only says to look now.
type Queue interface {
	Enqueue(reference string)
}

func register(mux *http.ServeMux, h *handlers) {
	mux.HandleFunc("GET /api/v1/health", h.health)

	// Public: no session required, and no volunteer or card data in the
	// response -- this is what the unauthenticated landing page reads.
	mux.HandleFunc("GET /api/v1/public/overview", h.publicOverview)

	// Session.
	mux.HandleFunc("GET /api/v1/session", h.getSession)
	mux.HandleFunc("POST /api/v1/session", h.createSession)
	mux.HandleFunc("DELETE /api/v1/session", h.deleteSession)

	// Volunteer.
	mux.HandleFunc("GET /api/v1/stations", h.requireSession(h.listStations))
	mux.HandleFunc("GET /api/v1/uploads", h.requireSession(h.listUploads))
	mux.HandleFunc("POST /api/v1/uploads", h.requireSession(h.createUpload))
	mux.HandleFunc("GET /api/v1/uploads/{reference}", h.requireSession(h.getUpload))
	mux.HandleFunc("POST /api/v1/uploads/{reference}/progress", h.requireSession(h.recordProgress))
	// Card audio, one tus upload per file (see tus.go). tusd routes POST, HEAD
	// and PATCH itself, so the pattern has no method.
	mux.Handle(tusPath, h.tusEndpoint())

	// Admin.
	mux.HandleFunc("GET /api/v1/admin/uploads", h.requireRole(db.RoleAdmin, h.listAllUploads))
	mux.HandleFunc("GET /api/v1/admin/uploads/{reference}", h.requireRole(db.RoleAdmin, h.getCardFiles))
	mux.HandleFunc("GET /api/v1/admin/uploads/{reference}/files/{file}/detections", h.requireRole(db.RoleAdmin, h.listFileDetections))
	mux.HandleFunc("GET /api/v1/admin/uploads/{reference}/detections/{id}", h.requireRole(db.RoleAdmin, h.getDetection))
	mux.HandleFunc("GET /api/v1/admin/uploads/{reference}/detections/{id}/clip", h.requireRole(db.RoleAdmin, h.getClip))
	mux.HandleFunc("PUT /api/v1/admin/uploads/{reference}/detections/{id}/review", h.requireRole(db.RoleAdmin, h.reviewDetection))
	mux.HandleFunc("GET /api/v1/admin/people", h.requireRole(db.RoleAdmin, h.listPeople))
	mux.HandleFunc("POST /api/v1/admin/people", h.requireRole(db.RoleAdmin, h.addPerson))
	mux.HandleFunc("PUT /api/v1/admin/people/{id}", h.requireRole(db.RoleAdmin, h.updatePerson))
	mux.HandleFunc("DELETE /api/v1/admin/people/{id}", h.requireRole(db.RoleAdmin, h.removePerson))
	mux.HandleFunc("POST /api/v1/admin/stations", h.requireRole(db.RoleAdmin, h.addStation))
	mux.HandleFunc("PUT /api/v1/admin/stations/{id}", h.requireRole(db.RoleAdmin, h.updateStation))
	mux.HandleFunc("DELETE /api/v1/admin/stations/{id}", h.requireRole(db.RoleAdmin, h.removeStation))

	// Development only: not registered at all otherwise, so a deployed server
	// 404s instead of handing the roster to anyone who asks.
	if h.dev {
		mux.HandleFunc("GET /api/v1/dev/people", h.listDevPeople)
	}

	// Anything else under /api/ is a 404 as JSON, not as the frontend's
	// index.html -- a mistyped API path should look like an API error.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		h.problem(w, http.StatusNotFound, "not found")
	})
}

type handlers struct {
	store db.Store
	// files is where card audio is stored.
	files storage.Store
	// queue hears about cards that are ready for BirdNET. Nil runs no analysis.
	queue Queue
	log   *slog.Logger
	// dev is set for local development; see Register.
	dev bool
	// now is the clock, so tests can pin "this year" and "the last 7 nights".
	now func() time.Time
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	h.json(w, http.StatusOK, map[string]string{"status": "ok"})
}

// stamp is the current instant as the store keeps it: UTC, whole seconds.
func (h *handlers) stamp() time.Time {
	return h.now().UTC().Truncate(time.Second)
}

// --- public ---

func (h *handlers) publicOverview(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 && n <= 30 {
			days = n
		}
	}
	now := h.now()
	program, species, err := overview(r.Context(), h.store, now, days)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.json(w, http.StatusOK, map[string]any{
		"windowDays": days,
		"updatedAt":  now.UTC().Truncate(time.Minute),
		"program":    program,
		"species":    species,
	})
}

// --- session ---

func (h *handlers) getSession(w http.ResponseWriter, r *http.Request) {
	// dev tells the sign-in page whether to offer the account picker.
	var user any
	me, ok, err := h.signedIn(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ok {
		user = personOf(me)
	}
	h.json(w, http.StatusOK, map[string]any{"user": user, "dev": h.dev})
}

func (h *handlers) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Provider string `json:"provider"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	var (
		me  db.User
		err error
	)
	switch {
	case body.Email != "":
		me, err = h.store.GetUserByEmail(ctx, body.Email)
	case body.Role != "":
		// PLACEHOLDER: "sign in as an admin" with no identity provider behind
		// it. Goes away with the first real OIDC callback.
		me, err = h.firstWithRole(r, body.Role)
	default:
		err = db.ErrNotFound
	}
	if err == nil {
		me, err = h.store.UpdateUser(ctx, me.ID, func(u *db.User) error {
			if u.RemovedAt != nil {
				return db.ErrNotFound
			}
			t := h.stamp()
			u.LastSignInAt = &t
			return nil
		})
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusForbidden, "that address isn't on the roster yet")
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}

	setSession(w, me)
	h.json(w, http.StatusOK, map[string]any{"user": personOf(me)})
}

// firstWithRole is how the placeholder sign-in picks an account: there is no
// identity provider wired up, so "sign in as an admin" means "be the first
// admin on the roster", by name.
func (h *handlers) firstWithRole(r *http.Request, role string) (db.User, error) {
	roster, err := h.roster(r)
	if err != nil {
		return db.User{}, err
	}
	for _, u := range roster {
		if u.Role == role {
			return u, nil
		}
	}
	return db.User{}, db.ErrNotFound
}

func setSession(w http.ResponseWriter, u db.User) {
	// Volunteers sign in once and are left alone for the season.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: u.Email, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 90 * 24 * 3600,
	})
}

// listDevPeople is the whole roster, for the development sign-in picker. It is
// deliberately unauthenticated: you pick from it before you have a session.
func (h *handlers) listDevPeople(w http.ResponseWriter, r *http.Request) {
	h.listRoster(w, r)
}

func (h *handlers) deleteSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	h.json(w, http.StatusOK, map[string]any{"user": nil})
}

// signedIn resolves the session cookie to someone on the roster. Someone who
// has been removed is signed out by it.
func (h *handlers) signedIn(r *http.Request) (db.User, bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return db.User{}, false, nil
	}
	u, err := h.store.GetUserByEmail(r.Context(), c.Value)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.User{}, false, nil
	case err != nil:
		return db.User{}, false, err
	}
	return u, u.RemovedAt == nil, nil
}

// requireSession rejects anonymous callers. requireRole additionally checks the
// role, so an admin-only route can't be reached by guessing the path.
func (h *handlers) requireSession(next func(http.ResponseWriter, *http.Request, db.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok, err := h.signedIn(r)
		switch {
		case err != nil:
			h.fail(w, r, err)
		case !ok:
			h.problem(w, http.StatusUnauthorized, "sign in first")
		default:
			next(w, r, me)
		}
	}
}

func (h *handlers) requireRole(role string, next func(http.ResponseWriter, *http.Request, db.User)) http.HandlerFunc {
	return h.requireSession(func(w http.ResponseWriter, r *http.Request, me db.User) {
		if me.Role != role {
			h.problem(w, http.StatusForbidden, "admins only")
			return
		}
		next(w, r, me)
	})
}

// --- volunteer ---

// listStations is the recorders in the field; retired ones stay stored, for
// the cards that came from them, but aren't offered for new cards.
func (h *handlers) listStations(w http.ResponseWriter, r *http.Request, _ db.User) {
	recorders, err := h.store.ListRecorders(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stations := []Station{}
	for _, rec := range recorders {
		if rec.RetiredAt == nil {
			stations = append(stations, stationOf(rec))
		}
	}
	h.json(w, http.StatusOK, map[string]any{"stations": stations})
}

func (h *handlers) listUploads(w http.ResponseWriter, r *http.Request, me db.User) {
	h.writeUploads(w, r, db.UploadFilter{UserID: me.ID})
}

func (h *handlers) writeUploads(w http.ResponseWriter, r *http.Request, f db.UploadFilter) {
	uploads, err := h.store.ListUploads(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.json(w, http.StatusOK, map[string]any{"uploads": mapAll(uploads, uploadOf)})
}

// createUpload registers a card the volunteer is about to send, with the list
// of audio files the browser read off it. Its reference is derived from the
// pull date and the recorder, so registering the same card again is a resume:
// the list is refreshed and the files already in are kept.
func (h *handlers) createUpload(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		StationID string     `json:"stationId"`
		PulledOn  string     `json:"pulledOn"`
		Notes     string     `json:"notes"`
		Nights    []Night    `json:"nights"`
		Files     []CardFile `json:"files"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()

	rec, err := h.store.GetRecorder(ctx, body.StationID)
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && rec.RetiredAt != nil):
		h.problem(w, http.StatusBadRequest, "unknown station")
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if _, err := time.Parse("2006-01-02", body.PulledOn); err != nil {
		h.problem(w, http.StatusBadRequest, "pulledOn must be YYYY-MM-DD")
		return
	}
	if len(body.Nights) == 0 || len(body.Files) == 0 {
		h.problem(w, http.StatusBadRequest, "the card has no audio on it")
		return
	}
	nights := make([]db.Night, len(body.Nights))
	var files int
	var bytes int64
	for i, n := range body.Nights {
		if _, err := time.Parse("2006-01-02", n.Date); err != nil || n.Files < 0 || n.Bytes < 0 {
			h.problem(w, http.StatusBadRequest, "each night needs a YYYY-MM-DD date and non-negative counts")
			return
		}
		nights[i] = db.Night(n)
		files += n.Files
		bytes += n.Bytes
	}
	listed, problem := cardList(body.Files, files, bytes)
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	notes, started := strings.TrimSpace(body.Notes), h.stamp()
	ref := db.UploadID(body.PulledOn, rec.ID)

	u, err := h.store.CreateUpload(ctx, db.Upload{
		ID:         ref,
		RecorderID: rec.ID,
		Recorder:   db.RecorderSnapshot{Name: rec.Name, Latitude: rec.Latitude, Longitude: rec.Longitude},
		UserID:     me.ID, UserName: me.Name,
		PulledOn: body.PulledOn, Notes: notes, Nights: nights,
		FileCount: files, TotalBytes: bytes,
		Status: db.StatusInProgress, StartedAt: started,
	})
	if errors.Is(err, db.ErrConflict) {
		// Same card again. The recorder and volunteer copies stay as they were
		// when it was first registered.
		var sameList bool
		sameList, err = h.listedAsBefore(ctx, ref, listed)
		if err == nil {
			u, err = h.store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
				if u.UserID != me.ID {
					return errCardTaken
				}
				// A card that is already in keeps the list it was sent with, and
				// doesn't go back to being sent. A different list is another card
				// with the same recorder and pull date, and answering with the one
				// that's in would show the volunteer its counts as theirs.
				if !transferring(u.Status) {
					if !sameList {
						return errCardReceived
					}
					u.Notes = notes
					return nil
				}
				u.Notes = notes
				u.Nights, u.FileCount, u.TotalBytes = nights, files, bytes
				u.Status, u.StartedAt = db.StatusInProgress, started
				return nil
			})
		}
	}
	if err == nil && transferring(u.Status) {
		if err = h.registerFiles(ctx, u, listed); err == nil {
			u, err = h.tallyFiles(ctx, ref)
		}
	}
	var stored []db.AudioFile
	if err == nil {
		stored, err = h.store.ListAudioFiles(ctx, ref)
	}
	switch {
	case errors.Is(err, errCardTaken):
		h.problem(w, http.StatusConflict, "another volunteer has already registered this card")
	case errors.Is(err, errCardReceived):
		h.problem(w, http.StatusConflict,
			"a different card from this recorder, pulled on the same date, has already been uploaded; check the date you pulled the card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusCreated, map[string]any{"upload": uploadOf(u), "files": cardFilesOf(stored)})
	}
}

var (
	errCardTaken    = errors.New("card belongs to someone else")
	errCardReceived = errors.New("card has been received with a different file list")
)

// listedAsBefore reports whether a card's file list is the one it was last
// registered with: the same paths at the same sizes.
func (h *handlers) listedAsBefore(ctx context.Context, ref string, listed []db.AudioFile) (bool, error) {
	existing, err := h.store.ListAudioFiles(ctx, ref)
	if err != nil {
		return false, err
	}
	sizes := make(map[string]int64, len(existing))
	for _, f := range existing {
		if f.StatusDetail != db.AudioDetailNotOnCard {
			sizes[f.Path] = f.SizeBytes
		}
	}
	if len(sizes) != len(listed) {
		return false, nil
	}
	for _, f := range listed {
		if size, ok := sizes[f.Path]; !ok || size != f.SizeBytes {
			return false, nil
		}
	}
	return true, nil
}

// cardList checks a card's file list against the nights it was summed into,
// and returns it as audio files, or else what is wrong with it.
func cardList(files []CardFile, wantFiles int, wantBytes int64) ([]db.AudioFile, string) {
	listed := make([]db.AudioFile, 0, len(files))
	seen := make(map[string]bool, len(files))
	var bytes int64
	for _, f := range files {
		p := db.CardPath(f.Path)
		if _, err := time.Parse("2006-01-02", f.Night); err != nil || p == "" || f.Bytes < 0 {
			return nil, "each file needs its path on the card, a non-negative size and a YYYY-MM-DD night"
		}
		if seen[p] {
			return nil, fmt.Sprintf("the file list has %s twice", p)
		}
		seen[p] = true
		bytes += f.Bytes
		listed = append(listed, db.AudioFile{Path: p, SizeBytes: f.Bytes, Night: f.Night})
	}
	if len(listed) != wantFiles || bytes != wantBytes {
		return nil, "the file list and the nights don't add up to the same card"
	}
	return listed, ""
}

// registerFiles makes a card's audio files match the list the browser has just
// read off it. A file that is already in stays in if it is the same size, and
// everything else listed is pending. A file that is no longer listed is marked
// failed rather than deleted, so whatever was stored for it is still accounted
// for.
func (h *handlers) registerFiles(ctx context.Context, u db.Upload, listed []db.AudioFile) error {
	existing, err := h.store.ListAudioFiles(ctx, u.ID)
	if err != nil {
		return err
	}
	unlisted := make(map[string]db.AudioFile, len(existing))
	for _, f := range existing {
		unlisted[f.ID] = f
	}
	var writes []db.AudioFile
	for _, f := range listed {
		id := db.AudioFileID(u.ID, f.Path)
		old, had := unlisted[id]
		delete(unlisted, id)
		unchanged := old.SizeBytes == f.SizeBytes && (received(old) || (old.Status == db.AudioPending && old.Night == f.Night))
		if had && unchanged {
			continue
		}
		writes = append(writes, db.AudioFile{
			ID: id, RecorderID: u.RecorderID, Path: f.Path, SizeBytes: f.SizeBytes,
			Night: f.Night, Status: db.AudioPending, CreatedAt: old.CreatedAt,
		})
	}
	for _, old := range unlisted {
		if old.StatusDetail != db.AudioDetailNotOnCard {
			old.Status, old.StatusDetail = db.AudioFailed, db.AudioDetailNotOnCard
			writes = append(writes, old)
		}
	}
	if len(writes) == 0 {
		return nil
	}
	return h.store.UpsertAudioFiles(ctx, u.ID, writes)
}

// tallyFiles sets a card's uploaded counts from its audio files, and moves a
// card whose every listed file has landed on to processing. The counts are
// recounted rather than added to, so a retried request can't count a file
// twice, and the browser never reports them.
func (h *handlers) tallyFiles(ctx context.Context, ref string) (db.Upload, error) {
	files, err := h.store.ListAudioFiles(ctx, ref)
	if err != nil {
		return db.Upload{}, err
	}
	var in, waiting int
	var bytes int64
	for _, f := range files {
		switch {
		case received(f):
			in++
			bytes += f.SizeBytes
		case f.Status == db.AudioPending:
			waiting++
		}
	}
	now := h.stamp()
	u, err := h.store.UpdateUpload(ctx, ref, func(u *db.Upload) error {
		if !transferring(u.Status) {
			return nil
		}
		u.FilesUploaded, u.BytesUploaded = min(in, u.FileCount), min(bytes, u.TotalBytes)
		if waiting == 0 && in > 0 {
			u.Status, u.ReceivedAt = db.StatusProcessing, &now
		}
		return nil
	})
	if err == nil && u.Status == db.StatusProcessing && h.queue != nil {
		h.queue.Enqueue(ref)
	}
	return u, err
}

// transferring reports whether a card's files are still being sent, which are
// the only states the client gets a say in.
func transferring(status string) bool {
	return status == db.StatusInProgress || status == db.StatusInterrupted
}

// canSee is the card rule: a volunteer sees their own, an admin sees every one.
func canSee(me db.User, u db.Upload) bool {
	return me.Role == db.RoleAdmin || u.UserID == me.ID
}

// getUpload is one card with the files on its list and where each stands, so a
// volunteer finishing a card can be told which files it already has before the
// card is registered again.
func (h *handlers) getUpload(w http.ResponseWriter, r *http.Request, me db.User) {
	ctx := r.Context()
	u, err := h.store.GetUpload(ctx, r.PathValue("reference"))
	var files []db.AudioFile
	if err == nil && canSee(me, u) {
		files, err = h.store.ListAudioFiles(ctx, u.ID)
	}
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && !canSee(me, u)):
		h.problem(w, http.StatusNotFound, "no such card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u), "files": cardFilesOf(files)})
	}
}

// recordProgress is the browser saying a transfer has stopped or started
// again, which is what the volunteer's home page shows. What has landed is
// counted by the server as each file arrives (afterFileUpload), never reported
// by the browser.
func (h *handlers) recordProgress(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if !transferring(body.Status) {
		h.problem(w, http.StatusBadRequest, "status must be in_progress or interrupted")
		return
	}

	u, err := h.store.UpdateUpload(r.Context(), r.PathValue("reference"), func(u *db.Upload) error {
		if !canSee(me, *u) {
			return db.ErrNotFound
		}
		// A card that is in stays in; the client never moves it along.
		if transferring(u.Status) {
			u.Status = body.Status
		}
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, "no such card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u)})
	}
}

// --- admin ---

func (h *handlers) listAllUploads(w http.ResponseWriter, r *http.Request, _ db.User) {
	h.writeUploads(w, r, db.UploadFilter{})
}

// getCardFiles is one card with every file on it, where each stands in upload
// and analysis, for the coordinator's card page.
func (h *handlers) getCardFiles(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	u, err := h.store.GetUpload(ctx, r.PathValue("reference"))
	var files []db.AudioFile
	if err == nil {
		files, err = h.store.ListAudioFiles(ctx, u.ID)
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, "no such card")
	case err != nil:
		h.fail(w, r, err)
	default:
		out := []AudioFile{}
		for _, f := range files {
			if f.StatusDetail != db.AudioDetailNotOnCard {
				out = append(out, audioFileOf(f))
			}
		}
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u), "files": out})
	}
}

// listFileDetections is everything BirdNET heard in one file, in the order it
// was heard.
func (h *handlers) listFileDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	ref := r.PathValue("reference")
	f, err := h.store.GetAudioFile(ctx, ref, r.PathValue("file"))
	var found []db.Detection
	if err == nil {
		found, err = h.store.ListDetections(ctx, db.DetectionFilter{UploadID: ref, AudioFileID: f.ID})
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, "no such file on that card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"detections": mapAll(found, detectionOf)})
	}
}

const msgNoSuchDetection = "no such detection on that card"

// getDetection is one detection, with the card and the file it was heard in,
// for the detection's own page.
func (h *handlers) getDetection(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	ref := r.PathValue("reference")
	d, err := h.store.GetDetection(ctx, ref, r.PathValue("id"))
	var u db.Upload
	var f db.AudioFile
	if err == nil {
		u, err = h.store.GetUpload(ctx, ref)
	}
	if err == nil {
		f, err = h.store.GetAudioFile(ctx, ref, d.AudioFileID)
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchDetection)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u), "file": audioFileOf(f), "detection": detectionOf(d)})
	}
}

// maxClipBytes is the most of a clip getClip will read. The analysis queue
// cuts at most 30 s of mono 16-bit audio, which is under 6 MB even at 96 kHz.
const maxClipBytes = 16 << 20

// getClip serves a detection's clip as a WAV. The clip is read whole so that
// http.ServeContent can answer range requests: browsers ask for audio in
// ranges, and Safari won't play a file served without them.
func (h *handlers) getClip(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	d, err := h.store.GetDetection(ctx, r.PathValue("reference"), r.PathValue("id"))
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchDetection)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	case d.Clip == nil:
		h.problem(w, http.StatusNotFound, "this detection has no clip; it was analyzed before clips were cut")
		return
	}
	audio, err := h.files.Open(ctx, d.Clip.BlobName)
	if errors.Is(err, storage.ErrNotFound) {
		h.problem(w, http.StatusNotFound, "this detection's clip isn't in storage")
		return
	}
	var body []byte
	if err == nil {
		body, err = io.ReadAll(io.LimitReader(audio, maxClipBytes+1))
		audio.Close()
	}
	if err == nil && len(body) > maxClipBytes {
		err = fmt.Errorf("clip %s is over %d bytes", d.Clip.BlobName, maxClipBytes)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	// Analyzing a file again cuts its clips again, under the same names.
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// reviewDetection records a verdict on a detection: confirmed, rejected (the
// page's "Discard"), or back to unreviewed. A review replaces the one before.
func (h *handlers) reviewDetection(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	switch body.Status {
	case db.ReviewConfirmed, db.ReviewRejected, db.ReviewUnreviewed:
	default:
		h.problem(w, http.StatusBadRequest, "status must be confirmed, rejected or unreviewed")
		return
	}
	at := h.stamp()
	d, err := h.store.UpdateDetection(r.Context(), r.PathValue("reference"), r.PathValue("id"), func(d *db.Detection) error {
		d.ReviewStatus, d.Review = body.Status, nil
		if body.Status != db.ReviewUnreviewed {
			d.Review = &db.Review{UserID: me.ID, UserName: me.Name, At: at}
		}
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchDetection)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"detection": detectionOf(d)})
	}
}

func (h *handlers) listPeople(w http.ResponseWriter, r *http.Request, _ db.User) {
	h.listRoster(w, r)
}

func (h *handlers) listRoster(w http.ResponseWriter, r *http.Request) {
	roster, err := h.roster(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.json(w, http.StatusOK, map[string]any{"people": mapAll(roster, personOf)})
}

// roster is everyone who hasn't been removed, sorted by name.
func (h *handlers) roster(r *http.Request) ([]db.User, error) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		return nil, err
	}
	out := users[:0]
	for _, u := range users {
		if u.RemovedAt == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// Roster messages, shared by the handlers that can hit them.
const (
	msgNoSuchPerson = "that person isn't on the roster"
	msgEmailTaken   = "that address is already on the roster"
	msgEmailRemoved = "that address belongs to someone who was removed from the roster; add them again instead"
	msgLastAdmin    = "the roster needs an admin; make someone else an admin first"
	msgRemoveSelf   = "you can't remove yourself from the roster"
)

// addPerson puts someone on the roster. An address that belongs to someone who
// was removed brings that person back, so their old cards are theirs again.
func (h *handlers) addPerson(w http.ResponseWriter, r *http.Request, _ db.User) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	email, name := strings.TrimSpace(body.Email), strings.TrimSpace(body.Name)
	if !strings.Contains(email, "@") {
		h.problem(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	if name == "" {
		name = email
	}
	role := body.Role
	if role != db.RoleAdmin {
		role = db.RoleVolunteer
	}

	ctx := r.Context()
	existing, err := h.store.GetUserByEmail(ctx, email)
	var u db.User
	switch {
	case errors.Is(err, db.ErrNotFound):
		u, err = h.store.CreateUser(ctx, db.User{Email: email, Name: name, Role: role})
	case err == nil && existing.RemovedAt == nil:
		err = db.ErrConflict
	case err == nil:
		u, err = h.store.UpdateUser(ctx, existing.ID, func(u *db.User) error {
			if u.RemovedAt == nil {
				return db.ErrConflict // someone else brought them back first
			}
			u.RemovedAt, u.Name, u.Role = nil, name, role
			return nil
		})
	}
	switch {
	case errors.Is(err, db.ErrConflict):
		h.problem(w, http.StatusConflict, msgEmailTaken)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusCreated, map[string]any{"person": personOf(u)})
	}
}

// updatePerson changes what a coordinator can change about someone: name,
// address and role. The provider and the day they were added are facts.
func (h *handlers) updatePerson(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	email, name := strings.TrimSpace(body.Email), strings.TrimSpace(body.Name)
	if !strings.Contains(email, "@") {
		h.problem(w, http.StatusBadRequest, "a valid email address is required")
		return
	}
	// Unlike addPerson, an unknown role is an error rather than "volunteer":
	// quietly demoting an admin is not a safe default for an edit.
	if body.Role != db.RoleAdmin && body.Role != db.RoleVolunteer {
		h.problem(w, http.StatusBadRequest, "role must be volunteer or admin")
		return
	}
	if name == "" {
		name = email
	}

	id := r.PathValue("id")
	if status, msg, err := h.checkAdminLeaves(r, id, body.Role == db.RoleAdmin); err != nil {
		h.fail(w, r, err)
		return
	} else if status != 0 {
		h.problem(w, status, msg)
		return
	}
	u, err := h.store.UpdateUser(r.Context(), id, func(u *db.User) error {
		if u.RemovedAt != nil {
			return db.ErrNotFound
		}
		u.Name, u.Email, u.Role = name, email, body.Role
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchPerson)
		return
	case errors.Is(err, db.ErrConflict):
		msg := msgEmailTaken
		if holder, err := h.store.GetUserByEmail(r.Context(), email); err == nil && holder.RemovedAt != nil {
			msg = msgEmailRemoved
		}
		h.problem(w, http.StatusConflict, msg)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	// The session cookie is the address, so re-addressing yourself would sign
	// you out mid-edit. Reissue it for the new one.
	if u.ID == me.ID {
		setSession(w, u)
	}
	h.json(w, http.StatusOK, map[string]any{"person": personOf(u)})
}

// removePerson takes someone off the roster. The document stays, with
// removedAt set, so their cards and reviews still resolve.
func (h *handlers) removePerson(w http.ResponseWriter, r *http.Request, me db.User) {
	id := r.PathValue("id")
	if id == me.ID {
		h.problem(w, http.StatusConflict, msgRemoveSelf)
		return
	}
	if status, msg, err := h.checkAdminLeaves(r, id, false); err != nil {
		h.fail(w, r, err)
		return
	} else if status != 0 {
		h.problem(w, status, msg)
		return
	}
	now := h.stamp()
	_, err := h.store.UpdateUser(r.Context(), id, func(u *db.User) error {
		if u.RemovedAt != nil {
			return db.ErrNotFound
		}
		u.RemovedAt = &now
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, msgNoSuchPerson)
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]string{"removed": id})
	}
}

// checkAdminLeaves enforces "the roster always keeps an admin" before person
// id is edited or removed: stillAdmin says whether they remain one afterwards.
// It answers a status and message to refuse with, or 0 to go ahead.
//
// The check and the write that follows are separate documents, and Cosmos has
// no transaction across partitions, so two admins demoting each other in the
// same instant could both pass. Like email uniqueness (SCHEMA.md), that race is
// accepted on a small, admin-only roster.
func (h *handlers) checkAdminLeaves(r *http.Request, id string, stillAdmin bool) (int, string, error) {
	target, err := h.store.GetUser(r.Context(), id)
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && target.RemovedAt != nil):
		return http.StatusNotFound, msgNoSuchPerson, nil
	case err != nil:
		return 0, "", err
	case target.Role != db.RoleAdmin || stillAdmin:
		return 0, "", nil
	}
	roster, err := h.roster(r)
	if err != nil {
		return 0, "", err
	}
	admins := 0
	for _, u := range roster {
		if u.Role == db.RoleAdmin {
			admins++
		}
	}
	if admins <= 1 {
		return http.StatusConflict, msgLastAdmin, nil
	}
	return 0, "", nil
}

// addStation puts a recorder in the field. The id is printed on the unit, so
// the coordinator types it. A retired unit's id brings that recorder back at
// its new name and place.
func (h *handlers) addStation(w http.ResponseWriter, r *http.Request, _ db.User) {
	var body struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if problem := stationProblem(body.Name, body.Latitude, body.Longitude); problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}

	ctx := r.Context()
	all, err := h.store.ListRecorders(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = fmt.Sprintf("SW-%02d", len(all)+1)
	}
	// Ids are compared case-insensitively: "sw-02" is the unit labelled SW-02.
	var existing *db.Recorder
	for i := range all {
		if strings.EqualFold(all[i].ID, id) {
			existing = &all[i]
		}
	}

	var rec db.Recorder
	switch {
	case existing == nil:
		rec, err = h.store.CreateRecorder(ctx, db.Recorder{ID: id, Name: body.Name, Latitude: body.Latitude, Longitude: body.Longitude})
	case existing.RetiredAt == nil:
		err = db.ErrConflict
	default:
		rec, err = h.store.UpdateRecorder(ctx, existing.ID, func(rec *db.Recorder) error {
			if rec.RetiredAt == nil {
				return db.ErrConflict
			}
			rec.RetiredAt, rec.Name, rec.Latitude, rec.Longitude = nil, body.Name, body.Latitude, body.Longitude
			return nil
		})
	}
	switch {
	case errors.Is(err, db.ErrConflict):
		if existing != nil {
			id = existing.ID
		}
		h.problem(w, http.StatusConflict, fmt.Sprintf("recorder %s is already in the field", id))
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusCreated, map[string]any{"station": stationOf(rec)})
	}
}

// updateStation renames or moves a recorder. The id is printed on the unit, so
// it is only in the path: a mistyped id is fixed by deleting and re-adding.
// Cards already sent keep the name and place they were recorded under.
func (h *handlers) updateStation(w http.ResponseWriter, r *http.Request, _ db.User) {
	var body struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if problem := stationProblem(body.Name, body.Latitude, body.Longitude); problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	h.changeStation(w, r, func(rec *db.Recorder) {
		rec.Name, rec.Latitude, rec.Longitude = body.Name, body.Latitude, body.Longitude
	}, func(rec db.Recorder) {
		h.json(w, http.StatusOK, map[string]any{"station": stationOf(rec)})
	})
}

// removeStation retires a recorder. The document stays, for the cards that
// came from it.
func (h *handlers) removeStation(w http.ResponseWriter, r *http.Request, _ db.User) {
	now := h.stamp()
	h.changeStation(w, r, func(rec *db.Recorder) {
		rec.RetiredAt = &now
	}, func(rec db.Recorder) {
		h.json(w, http.StatusOK, map[string]string{"removed": rec.ID})
	})
}

// changeStation applies change to the recorder in the path, if it is still in
// the field, and hands the result to done.
func (h *handlers) changeStation(w http.ResponseWriter, r *http.Request, change func(*db.Recorder), done func(db.Recorder)) {
	rec, err := h.store.UpdateRecorder(r.Context(), r.PathValue("id"), func(rec *db.Recorder) error {
		if rec.RetiredAt != nil {
			return db.ErrNotFound
		}
		change(rec)
		return nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, "no recorder has that id")
	case err != nil:
		h.fail(w, r, err)
	default:
		done(rec)
	}
}

// stationProblem is what's wrong with a recorder's name and position, or "".
func stationProblem(name string, lat, lon float64) string {
	switch {
	case name == "":
		return "a station name is required"
	case lat == 0 && lon == 0:
		return "place the recorder on the map first"
	case lat < -90 || lat > 90 || lon < -180 || lon > 180:
		return "those coordinates aren't on the map"
	}
	return ""
}

// --- plumbing ---

// decode reads a JSON request body, refusing anything unreasonably large or
// shaped unexpectedly rather than silently ignoring it.
func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("a JSON body is required")
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("could not read the request body as JSON")
	}
	return nil
}

func (h *handlers) json(w http.ResponseWriter, status int, body any) {
	writeJSON(w, h.log, status, body)
}

// problem answers with an error the client is meant to show.
func (h *handlers) problem(w http.ResponseWriter, status int, msg string) {
	h.json(w, status, map[string]string{"error": msg})
}

// fail answers an error nobody planned for, usually the database. The detail
// goes to the log, not to the client.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	h.problem(w, http.StatusInternalServerError, "something went wrong on our side; try again")
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error("encode response", "err", err)
	}
}
