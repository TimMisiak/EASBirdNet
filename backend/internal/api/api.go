// Package api holds the JSON HTTP API. Everything it serves lives under
// /api/v1/ so the static frontend can own every other path.
//
// Handlers read and write through db.Store and answer in the shapes in
// shapes.go. Sign-in is OpenID Connect against Google and Microsoft (auth.go);
// the roster is the allow-list behind it.
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
	"sync"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/analysis"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/retention"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// sessionCookie carries the signed-in person's email, signed with the server's
// key (cookies.go) so it can't be edited into someone else's.
const sessionCookie = "bs_session"

// sessionLife is how long a signed-in volunteer is left alone. They sign in
// once and send cards for the season.
const sessionLife = 90 * 24 * time.Hour

// Options is everything the API needs from the rest of the server.
type Options struct {
	Store db.Store
	// Files is where card audio goes.
	Files storage.Store
	// Queue is told when a card has all its files, so BirdNET can start on it.
	Queue Queue
	Log   *slog.Logger
	// Dev turns on the development-only routes -- the roster the sign-in page
	// lets you pick an account from, and the sign-in that goes with it.
	Dev bool
	// Auth is OIDC sign-in. Nil offers none, which only dev mode survives.
	Auth *Authenticator
	// SessionKey signs the session cookie. Changing it signs everyone out.
	SessionKey string
	// Retention is how long a card's originals are kept, so a card can say
	// when its audio is due to go. The zero value keeps them for good.
	Retention retention.Policy
}

// Register mounts the API routes on mux.
func Register(mux *http.ServeMux, o Options) {
	register(mux, &handlers{
		store: o.Store, files: o.Files, queue: o.Queue, log: o.Log, dev: o.Dev,
		auth: o.Auth, keys: newKeyset(o.SessionKey), secureCookies: !o.Dev,
		retention: o.Retention, now: time.Now,
	})
}

// Queue is the analysis queue (internal/analysis). A received card is queued
// by its status already; Enqueue only says to look now, and Status says
// whether anything is looking at all.
type Queue interface {
	Enqueue(reference string)
	Status() analysis.Status
}

func register(mux *http.ServeMux, h *handlers) {
	h.overview = newOverviewCache()

	mux.HandleFunc("GET /api/v1/health", h.health)
	mux.HandleFunc("GET /api/v1/ready", h.ready)

	// Public: no session required, and no volunteer or card data in the
	// response -- this is what the unauthenticated landing page reads.
	mux.HandleFunc("GET /api/v1/public/overview", h.publicOverview)

	// Session.
	mux.HandleFunc("GET /api/v1/session", h.getSession)
	mux.HandleFunc("DELETE /api/v1/session", h.deleteSession)

	// Sign-in: a browser is redirected through these, so they answer with
	// redirects rather than JSON (auth.go).
	if h.auth != nil {
		mux.HandleFunc("GET /api/v1/auth/{provider}/start", h.authStart)
		mux.HandleFunc("GET /api/v1/auth/{provider}/callback", h.authCallback)
	}

	// Volunteer.
	mux.HandleFunc("GET /api/v1/stations", h.requireSession(h.listStations))
	mux.HandleFunc("GET /api/v1/uploads", h.requireSession(h.listUploads))
	mux.HandleFunc("POST /api/v1/uploads", h.requireSession(h.createUpload))
	mux.HandleFunc("GET /api/v1/uploads/{reference}", h.requireSession(h.getUpload))
	mux.HandleFunc("POST /api/v1/uploads/{reference}/progress", h.requireSession(h.recordProgress))
	// Detections are anyone's to hear and review once signed in, on every card.
	mux.HandleFunc("GET /api/v1/detections", h.requireSession(h.listDetections))
	mux.HandleFunc("GET /api/v1/detections/{reference}", h.requireSession(h.listCardDetections))
	mux.HandleFunc("GET /api/v1/detections/{reference}/{id}", h.requireSession(h.getDetection))
	mux.HandleFunc("GET /api/v1/detections/{reference}/{id}/clip", h.requireSession(h.getClip))
	mux.HandleFunc("PUT /api/v1/detections/{reference}/{id}/review", h.requireSession(h.reviewDetection))
	// Card audio, one tus upload per file (see tus.go). tusd routes POST, HEAD
	// and PATCH itself, so the pattern has no method.
	mux.Handle(tusPath, h.tusEndpoint())

	// Admin.
	mux.HandleFunc("GET /api/v1/admin/uploads", h.requireRole(db.RoleAdmin, h.listAllUploads))
	mux.HandleFunc("GET /api/v1/admin/uploads/{reference}", h.requireRole(db.RoleAdmin, h.getCardFiles))
	mux.HandleFunc("DELETE /api/v1/admin/uploads/{reference}", h.requireRole(db.RoleAdmin, h.deleteUpload))
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
		// Signing in as anyone on the roster, with no provider behind it. This
		// is the development picker, and it is why it is registered here and
		// not at all otherwise: a deployed server has no way in but OIDC.
		mux.HandleFunc("POST /api/v1/session", h.createSession)
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
	// auth is OIDC sign-in, or nil when none is configured.
	auth *Authenticator
	// keys signs the session and sign-in cookies.
	keys *keyset
	// secureCookies keeps cookies to HTTPS. Off in dev, which is http://localhost.
	secureCookies bool
	// retention is how long card originals are kept, for the dates a card
	// reports. The sweep itself is internal/retention's.
	retention retention.Policy
	// now is the clock, so tests can pin "this year" and "the last 7 nights".
	now func() time.Time
	// overview holds the landing page's answer for a minute at a time. Set by
	// register, so every mux has one.
	overview *overviewCache
}

// health is unconditionally ok: it is the liveness and startup probe, and a
// dependency being down is not a reason to restart the container -- that would
// take the BirdNET run in flight with it and fix nothing. Readiness is ready
// below, which is the one that can fail. queue is the one thing health
// reports, as a bare state with no detail -- this route needs no session, and
// a card sitting in processing forever is the failure nobody would otherwise
// see from outside.
func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	h.json(w, http.StatusOK, map[string]string{"status": "ok", "queue": h.queueStatus().State})
}

// readyTimeout bounds the dependency checks, so a probe gets an answer rather
// than hanging on a store that has stopped answering. It has to stay well
// under the probe's own timeout (infra/app.tf).
const readyTimeout = 3 * time.Second

// ready is the readiness probe, and the only health route that can fail: a
// replica that can't reach Cosmos DB or Blob Storage has nothing useful to
// serve, so it should leave ingress rather than answer 500s until someone
// notices. It deliberately says nothing about the analysis queue -- a stuck
// queue is reported by health, and taking the site down over it would help
// nobody.
//
// The checks run together so one that is hanging can't spend the budget and
// make the other look broken. Each is named but never described: this route
// has no session, so what went wrong goes to the log and the response says
// only which dependency it was.
func (h *handlers) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	checks := []struct {
		name string
		ping func(context.Context) error
	}{
		{"database", h.store.Ping},
		{"storage", h.files.Ping},
	}
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	wg.Add(len(checks))
	for i, c := range checks {
		go func() {
			defer wg.Done()
			errs[i] = c.ping(ctx)
		}()
	}
	wg.Wait()

	body := map[string]string{"status": "ready"}
	code := http.StatusOK
	for i, c := range checks {
		body[c.name] = "ok"
		if errs[i] != nil {
			h.log.Error("not ready", "dependency", c.name, "err", errs[i])
			body[c.name] = "unreachable"
			body["status"] = "unready"
			code = http.StatusServiceUnavailable
		}
	}
	h.json(w, code, body)
}

// queueStatus is where analysis stands in this process. A nil queue is a
// server running none at all, so its cards would stay in processing: that is
// "off" rather than an error.
func (h *handlers) queueStatus() QueueStatus {
	if h.queue == nil {
		return QueueStatus{State: "off"}
	}
	s := h.queue.Status()
	out := QueueStatus{State: s.State, Detail: s.Detail}
	if !s.Since.IsZero() {
		since := s.Since.UTC()
		out.Since = &since
	}
	return out
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
	updatedAt := now.UTC().Truncate(time.Minute)
	program, species, err := h.overview.get(r.Context(), h.store, now, days)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The same answer stands, for everyone, until the minute it is stamped
	// with is over -- so a reload inside that minute needn't ask again. This is
	// the public page: there is nobody's data in it to keep out of a shared
	// cache.
	stands := updatedAt.Add(time.Minute).Sub(now) / time.Second
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(stands)))
	h.json(w, http.StatusOK, map[string]any{
		"windowDays": days,
		"updatedAt":  updatedAt,
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
	h.json(w, http.StatusOK, map[string]any{"user": user, "dev": h.dev, "providers": h.auth.Names()})
}

// createSession is the development sign-in: pick anyone on the roster, with no
// identity provider behind it. It is only registered in dev mode (see
// register), so a deployed server's only way in is OIDC.
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

	h.setSession(w, me)
	h.json(w, http.StatusOK, map[string]any{"user": personOf(me)})
}

// firstWithRole is how the development sign-in picks an account without being
// told an address: "sign in as an admin" means "be the first admin on the
// roster", by name.
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

func (h *handlers) setSession(w http.ResponseWriter, u db.User) {
	expires := h.now().Add(sessionLife)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: h.keys.sign(u.Email, expires), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secureCookies,
		MaxAge: int(sessionLife / time.Second),
	})
}

// listDevPeople is the whole roster, for the development sign-in picker. It is
// deliberately unauthenticated: you pick from it before you have a session.
func (h *handlers) listDevPeople(w http.ResponseWriter, r *http.Request) {
	h.listRoster(w, r)
}

func (h *handlers) deleteSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secureCookies, MaxAge: -1,
	})
	h.json(w, http.StatusOK, map[string]any{"user": nil})
}

// signedIn resolves the session cookie to someone on the roster. Someone who
// has been removed is signed out by it.
func (h *handlers) signedIn(r *http.Request) (db.User, bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return db.User{}, false, nil
	}
	// An edited or expired cookie is anonymous, not an error: the browser is
	// told to sign in again.
	email, err := h.keys.verify(c.Value, h.now())
	if err != nil {
		return db.User{}, false, nil
	}
	u, err := h.store.GetUserByEmail(r.Context(), email)
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
	h.writeUploads(w, r, db.UploadFilter{UserID: me.ID}, false)
}

// writeUploads answers a list of cards. Only the coordinator's list carries
// the analysis queue's state: its detail names server-side paths, and acting
// on it is a coordinator's job, not a volunteer's.
func (h *handlers) writeUploads(w http.ResponseWriter, r *http.Request, f db.UploadFilter, withQueue bool) {
	uploads, err := h.store.ListUploads(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := map[string]any{"uploads": mapAll(uploads, func(u db.Upload) Upload {
		return uploadOf(u, h.retention)
	})}
	if withQueue {
		body["queue"] = h.queueStatus()
	}
	h.json(w, http.StatusOK, body)
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

	// An id no recorder could have is looked up as a miss, not handed to the
	// store: Cosmos answers a read for an id containing '/' with a raw 400.
	if db.RecorderIDProblem(body.StationID) != "" {
		h.problem(w, http.StatusBadRequest, "unknown station")
		return
	}
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
		// A night is measured against the room the card has left rather than
		// added and then checked, so a crafted list can't overflow the sums
		// into a total small enough -- or negative enough -- to pass.
		if n.Files > maxCardFiles-files || n.Bytes > maxCardBytes-bytes {
			h.problem(w, http.StatusBadRequest, cardTooBig)
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
		h.json(w, http.StatusCreated, map[string]any{"upload": uploadOf(u, h.retention), "files": cardFilesOf(stored)})
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

// What a card may say it holds. A recorder fills a ~128 GB card with files of
// a few hundred MB, so these leave room for a larger card and for a whole
// night at a high sample rate in one file: they bound the absurd, not the
// unusual. They are worth having because a declared length is what the server
// then streams through the container into blob storage on a volunteer's word,
// and because keeping every value and every running total inside them is what
// stops the int64 sums from overflowing into a total that looks plausible.
const (
	maxFileBytes = 32 << 30 // 32 GiB, one file
	maxCardBytes = 1 << 40  // 1 TiB, the whole card
	maxCardFiles = 50_000   // one audioFiles document each
)

// cardTooBig is what a card claiming more than that is told.
const cardTooBig = "that is more audio than a card can hold; check the folder you chose"

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
		// Measured against what the card has left, for the reason the nights are.
		if f.Bytes > maxFileBytes || f.Bytes > maxCardBytes-bytes || len(listed) == maxCardFiles {
			return nil, cardTooBig
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
//
// A file document is the only thing that names what was stored for it, so a
// file that is about to be listed at a different length has its stored bytes
// deleted first: nothing else would ever find them again. The bytes go before
// the document that names them, the way retention does it, so a registration
// that fails part way leaves a file to delete again rather than storage
// nothing points at.
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
		sameFile := had && old.SizeBytes == f.SizeBytes
		if sameFile && (received(old) || (old.Status == db.AudioPending && old.Night == f.Night)) {
			continue
		}
		if sameFile && old.BlobName != "" {
			// A file that was taken off the card's list and is on it again at
			// the same length is the one already in storage, so it counts as
			// received rather than being sent a second time.
			old.Status, old.StatusDetail, old.Night = db.AudioUploaded, "", f.Night
			writes = append(writes, old)
			continue
		}
		if old.BlobName != "" {
			if err := h.files.Delete(ctx, old.BlobName); err != nil {
				return fmt.Errorf("api: replacing %s on %s: %w", f.Path, u.ID, err)
			}
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
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u, h.retention), "files": cardFilesOf(files)})
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
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u, h.retention)})
	}
}

// --- admin ---

func (h *handlers) listAllUploads(w http.ResponseWriter, r *http.Request, _ db.User) {
	h.writeUploads(w, r, db.UploadFilter{}, true)
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
		h.json(w, http.StatusOK, map[string]any{
			"upload": uploadOf(u, h.retention), "files": out, "queue": h.queueStatus(),
		})
	}
}

// deleteUpload removes a card for good: its audio, its audio files, the
// detections BirdNET found in them, and the card itself. The audio goes first
// and the card last, so a delete that fails part way leaves the card listed to
// be deleted again.
func (h *handlers) deleteUpload(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	u, err := h.store.GetUpload(ctx, r.PathValue("reference"))
	if err == nil {
		err = h.deleteAudio(ctx, u)
	}
	// Not found by now means the analysis queue, finding the card going, got to
	// the end of deleting it first.
	if err == nil {
		if err = h.store.DeleteUpload(ctx, u.ID); errors.Is(err, db.ErrNotFound) {
			err = nil
		}
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, "no such card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]string{"removed": u.ID})
	}
}

// deleteAudio removes everything storage holds for a card: its files, finished
// or not, and its clips, which is everything under its prefix. storagePrefix
// can spell two references the same, so if another card shares the prefix the
// audio is left where it is, rather than taking that card's with it.
func (h *handlers) deleteAudio(ctx context.Context, u db.Upload) error {
	prefix := storagePrefix(u.ID)
	all, err := h.store.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.ID != u.ID && storagePrefix(other.ID) == prefix {
			h.log.Warn("deleting a card: leaving its audio and clips in storage, another card shares its prefix",
				"upload", u.ID, "other", other.ID, "prefix", storage.Name(prefix))
			return nil
		}
	}
	return h.files.DeleteAll(ctx, prefix)
}

// listCardDetections is what BirdNET heard on a card, or with ?file= in one of
// its files, in the order it was heard, a page at a time: limit and offset
// read the same way as on the list of every detection, and total is how many
// there are in all. A whole card is tens of thousands of detections, which is
// a response nothing wants in one piece even though the query is a single
// partition and so cheap to run.
func (h *handlers) listCardDetections(w http.ResponseWriter, r *http.Request, _ db.User) {
	ctx := r.Context()
	ref := r.PathValue("reference")
	limit, offset, problem := parsePage(r.URL.Query())
	if problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	filter := db.DetectionFilter{UploadID: ref}
	missing := "no such card"
	var err error
	if id := r.URL.Query().Get("file"); id != "" {
		var f db.AudioFile
		f, err = h.store.GetAudioFile(ctx, ref, id)
		filter.AudioFileID, missing = f.ID, "no such file on that card"
	} else {
		_, err = h.store.GetUpload(ctx, ref)
	}
	var found []db.Detection
	if err == nil {
		found, err = h.store.ListDetections(ctx, filter)
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.problem(w, http.StatusNotFound, missing)
	case err != nil:
		h.fail(w, r, err)
	default:
		page := found[min(offset, len(found)):min(offset+limit, len(found))]
		h.json(w, http.StatusOK, map[string]any{"detections": mapAll(page, detectionOf), "total": len(found)})
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
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u, h.retention), "file": audioFileOf(f), "detection": detectionOf(d)})
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
		h.setSession(w, u)
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
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
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
	id := db.NormalizeRecorderID(body.ID)
	if id == "" {
		id = fmt.Sprintf("SW-%02d", len(all)+1)
	}
	if problem := db.RecorderIDProblem(id); problem != "" {
		h.problem(w, http.StatusBadRequest, problem)
		return
	}
	// Ids are compared case-insensitively: "sw-02" is the unit labelled SW-02,
	// and the stored form is the label's.
	var existing *db.Recorder
	for i := range all {
		switch {
		case strings.EqualFold(all[i].ID, id):
			existing = &all[i]
		case db.RecorderRef(all[i].ID) == db.RecorderRef(id):
			// Different recorders, one card reference: "02" and "SW-02" both
			// make OWL-20260907-SR02 out of the same pull date (db.UploadID).
			h.problem(w, http.StatusConflict, fmt.Sprintf("recorder %s would share card references with %s", id, all[i].ID))
			return
		}
	}

	var rec db.Recorder
	switch {
	case existing == nil:
		rec, err = h.store.CreateRecorder(ctx, db.Recorder{ID: id, Name: body.Name, Latitude: *body.Latitude, Longitude: *body.Longitude})
	case existing.RetiredAt == nil:
		err = db.ErrConflict
	default:
		rec, err = h.store.UpdateRecorder(ctx, existing.ID, func(rec *db.Recorder) error {
			if rec.RetiredAt == nil {
				return db.ErrConflict
			}
			rec.RetiredAt, rec.Name, rec.Latitude, rec.Longitude = nil, body.Name, *body.Latitude, *body.Longitude
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
		Name      string   `json:"name"`
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
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
		rec.Name, rec.Latitude, rec.Longitude = body.Name, *body.Latitude, *body.Longitude
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
// The coordinates are pointers so that a missing one is refused rather than
// read as 0: a browser's Number("") is 0 too, so a half-filled position would
// otherwise store a recorder in the Gulf of Guinea and send that to BirdNET's
// geo filter.
func stationProblem(name string, lat, lon *float64) string {
	switch {
	case name == "":
		return "a station name is required"
	case lat == nil || lon == nil:
		return "a latitude and a longitude are both required"
	case *lat == 0 && *lon == 0:
		return "place the recorder on the map first"
	case *lat < -90 || *lat > 90 || *lon < -180 || *lon > 180:
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
