// Package api holds the JSON HTTP API. Everything it serves lives under
// /api/v1/ so the static frontend can own every other path.
//
// Handlers read and write through db.Store and answer in the shapes in
// shapes.go. Sign-in is still a placeholder: it trusts the client (see
// sessionCookie).
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// sessionCookie carries the signed-in person's email.
//
// PLACEHOLDER AUTH: there is no identity provider wired up and nothing signs
// this cookie, so anyone can claim any roster address. Replace the whole
// session block with real OIDC against Google and Microsoft before this is
// reachable from outside a demo.
const sessionCookie = "bs_session"

// Register mounts the API routes on mux. dev turns on the development-only
// routes -- today, the roster the sign-in page lets you pick an account from.
func Register(mux *http.ServeMux, store db.Store, log *slog.Logger, dev bool) {
	register(mux, &handlers{store: store, log: log, dev: dev, now: time.Now})
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

	// Admin.
	mux.HandleFunc("GET /api/v1/admin/uploads", h.requireRole(db.RoleAdmin, h.listAllUploads))
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

// createUpload registers a card the volunteer is about to send. Its reference
// is derived from the pull date and the recorder, so registering the same card
// again is a resume: the manifest is refreshed and what already landed is kept.
func (h *handlers) createUpload(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		StationID string  `json:"stationId"`
		PulledOn  string  `json:"pulledOn"`
		Notes     string  `json:"notes"`
		Nights    []Night `json:"nights"`
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
	if len(body.Nights) == 0 {
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
	notes, started := strings.TrimSpace(body.Notes), h.stamp()

	u, err := h.store.CreateUpload(ctx, db.Upload{
		ID:         db.UploadID(body.PulledOn, rec.ID),
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
		u, err = h.store.UpdateUpload(ctx, db.UploadID(body.PulledOn, rec.ID), func(u *db.Upload) error {
			if u.UserID != me.ID {
				return errCardTaken
			}
			u.Notes, u.Nights, u.FileCount, u.TotalBytes = notes, nights, files, bytes
			// A card that is already in doesn't go back to being sent.
			if transferring(u.Status) {
				u.Status, u.StartedAt = db.StatusInProgress, started
			}
			return nil
		})
	}
	switch {
	case errors.Is(err, errCardTaken):
		h.problem(w, http.StatusConflict, "another volunteer has already registered this card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusCreated, map[string]any{"upload": uploadOf(u)})
	}
}

var errCardTaken = errors.New("card belongs to someone else")

// transferring reports whether a card's files are still being sent, which are
// the only states the client gets a say in.
func transferring(status string) bool {
	return status == db.StatusInProgress || status == db.StatusInterrupted
}

// canSee is the card rule: a volunteer sees their own, an admin sees every one.
func canSee(me db.User, u db.Upload) bool {
	return me.Role == db.RoleAdmin || u.UserID == me.ID
}

func (h *handlers) getUpload(w http.ResponseWriter, r *http.Request, me db.User) {
	u, err := h.store.GetUpload(r.Context(), r.PathValue("reference"))
	switch {
	case errors.Is(err, db.ErrNotFound) || (err == nil && !canSee(me, u)):
		h.problem(w, http.StatusNotFound, "no such card")
	case err != nil:
		h.fail(w, r, err)
	default:
		h.json(w, http.StatusOK, map[string]any{"upload": uploadOf(u)})
	}
}

// recordProgress is what the browser calls as each batch of files lands. The
// real version will be the storage backend's own callback; until then the
// client reports, and the counts only ever move forward.
func (h *handlers) recordProgress(w http.ResponseWriter, r *http.Request, me db.User) {
	var body struct {
		FilesUploaded int    `json:"filesUploaded"`
		BytesUploaded int64  `json:"bytesUploaded"`
		Status        string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		h.problem(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Status != "" && !transferring(body.Status) {
		h.problem(w, http.StatusBadRequest, "status must be in_progress or interrupted")
		return
	}

	now := h.stamp()
	u, err := h.store.UpdateUpload(r.Context(), r.PathValue("reference"), func(u *db.Upload) error {
		if !canSee(me, *u) {
			return db.ErrNotFound
		}
		u.FilesUploaded = max(u.FilesUploaded, min(body.FilesUploaded, u.FileCount))
		u.BytesUploaded = max(u.BytesUploaded, min(body.BytesUploaded, u.TotalBytes))
		if !transferring(u.Status) {
			return nil
		}
		if body.Status != "" {
			u.Status = body.Status
		}
		// A fully received card moves itself on to processing; the client never
		// gets to declare a card done.
		if u.FilesUploaded >= u.FileCount {
			u.Status, u.ReceivedAt = db.StatusProcessing, &now
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
