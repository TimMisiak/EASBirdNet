// Package api holds the JSON HTTP API. Everything it serves lives under
// /api/v1/ so the static frontend can own every other path.
//
// The handlers here are placeholders in one specific sense: the data behind
// them is in memory (see store.go) and sign-in trusts the client. The shapes
// are not placeholders -- the frontend is built against them.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Roles. A volunteer can upload cards; an admin can do that and manage the
// roster and the recorders.
const (
	RoleVolunteer = "volunteer"
	RoleAdmin     = "admin"
)

// sessionCookie carries the signed-in person's email.
//
// PLACEHOLDER AUTH: there is no identity provider wired up and nothing signs
// this cookie, so anyone can claim any roster address. Replace the whole
// session block with real OIDC against Google and Microsoft before this is
// reachable from outside a demo.
const sessionCookie = "bs_session"

// Register mounts the API routes on mux.
func Register(mux *http.ServeMux, log *slog.Logger) {
	s := newStore()
	h := &handlers{store: s, log: log}

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
	mux.HandleFunc("GET /api/v1/admin/uploads", h.requireRole(RoleAdmin, h.listAllUploads))
	mux.HandleFunc("GET /api/v1/admin/people", h.requireRole(RoleAdmin, h.listPeople))
	mux.HandleFunc("POST /api/v1/admin/people", h.requireRole(RoleAdmin, h.addPerson))
	mux.HandleFunc("POST /api/v1/admin/stations", h.requireRole(RoleAdmin, h.addStation))

	// Anything else under /api/ is a 404 as JSON, not as the frontend's
	// index.html -- a mistyped API path should look like an API error.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, log, http.StatusNotFound, map[string]string{"error": "not found"})
	})
}

type handlers struct {
	store *store
	log   *slog.Logger
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	h.json(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- public ---

func (h *handlers) publicOverview(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 && n <= 30 {
			days = n
		}
	}
	h.json(w, http.StatusOK, map[string]any{
		"windowDays": days,
		"updatedAt":  time.Now().UTC().Truncate(time.Minute),
		"program":    h.store.Program(),
		"species":    h.store.SpeciesSummary(),
	})
}

// --- session ---

func (h *handlers) getSession(w http.ResponseWriter, r *http.Request) {
	p, ok := h.person(r)
	if !ok {
		h.json(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	h.json(w, http.StatusOK, map[string]any{"user": p})
}

func (h *handlers) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Provider string `json:"provider"`
		// Remember keeps the session across browser restarts; volunteers are
		// asked once and then left alone for the season.
		Remember bool `json:"remember"`
	}
	if err := decode(r, &body); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var (
		p  Person
		ok bool
	)
	switch {
	case body.Email != "":
		p, ok = h.store.PersonByEmail(body.Email)
	case body.Role != "":
		// PLACEHOLDER: "sign in as an admin" with no identity provider behind
		// it. Goes away with the first real OIDC callback.
		p, ok = h.store.FirstWithRole(body.Role)
	}
	if !ok {
		h.json(w, http.StatusForbidden, map[string]string{
			"error": "that address isn't on the roster yet",
		})
		return
	}

	maxAge := 0 // a session cookie: gone when the browser closes
	if body.Remember {
		maxAge = 90 * 24 * 3600
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: p.Email, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
	h.json(w, http.StatusOK, map[string]any{"user": p})
}

func (h *handlers) deleteSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	h.json(w, http.StatusOK, map[string]any{"user": nil})
}

// person resolves the signed-in roster entry, if there is one.
func (h *handlers) person(r *http.Request) (Person, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Person{}, false
	}
	return h.store.PersonByEmail(c.Value)
}

// requireSession rejects anonymous callers. requireRole additionally checks the
// role, so an admin-only route can't be reached by guessing the path.
func (h *handlers) requireSession(next func(http.ResponseWriter, *http.Request, Person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.person(r)
		if !ok {
			h.json(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
			return
		}
		next(w, r, p)
	}
}

func (h *handlers) requireRole(role string, next func(http.ResponseWriter, *http.Request, Person)) http.HandlerFunc {
	return h.requireSession(func(w http.ResponseWriter, r *http.Request, p Person) {
		if p.Role != role {
			h.json(w, http.StatusForbidden, map[string]string{"error": "admins only"})
			return
		}
		next(w, r, p)
	})
}

// --- volunteer ---

func (h *handlers) listStations(w http.ResponseWriter, r *http.Request, _ Person) {
	h.json(w, http.StatusOK, map[string]any{"stations": h.store.Stations()})
}

func (h *handlers) listUploads(w http.ResponseWriter, r *http.Request, p Person) {
	h.json(w, http.StatusOK, map[string]any{"uploads": h.store.Uploads(p.Name)})
}

func (h *handlers) createUpload(w http.ResponseWriter, r *http.Request, p Person) {
	var body struct {
		StationID string  `json:"stationId"`
		PulledOn  string  `json:"pulledOn"`
		Notes     string  `json:"notes"`
		Nights    []Night `json:"nights"`
	}
	if err := decode(r, &body); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var station Station
	for _, st := range h.store.Stations() {
		if st.ID == body.StationID {
			station = st
		}
	}
	if station.ID == "" {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "unknown station"})
		return
	}
	if _, err := time.Parse("2006-01-02", body.PulledOn); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "pulledOn must be YYYY-MM-DD"})
		return
	}
	if len(body.Nights) == 0 {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "the card has no audio on it"})
		return
	}

	u := h.store.CreateUpload(Upload{
		StationID: station.ID, StationName: station.Name, VolunteerName: p.Name,
		PulledOn: body.PulledOn, Notes: strings.TrimSpace(body.Notes), Nights: body.Nights,
	})
	h.json(w, http.StatusCreated, map[string]any{"upload": u})
}

func (h *handlers) getUpload(w http.ResponseWriter, r *http.Request, p Person) {
	u, ok := h.store.Upload(r.PathValue("reference"))
	if !ok || (p.Role != RoleAdmin && u.VolunteerName != p.Name) {
		h.json(w, http.StatusNotFound, map[string]string{"error": "no such card"})
		return
	}
	h.json(w, http.StatusOK, map[string]any{"upload": u})
}

// recordProgress is what the browser calls as each batch of files lands. The
// real version will be the storage backend's own callback; until then the
// client reports and the store only ever moves the count forward.
func (h *handlers) recordProgress(w http.ResponseWriter, r *http.Request, p Person) {
	var body struct {
		FilesUploaded int    `json:"filesUploaded"`
		BytesUploaded int64  `json:"bytesUploaded"`
		Status        string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	switch body.Status {
	case "", StatusInProgress, StatusInterrupted:
	default:
		h.json(w, http.StatusBadRequest, map[string]string{"error": "status must be in_progress or interrupted"})
		return
	}

	u, ok := h.store.Upload(r.PathValue("reference"))
	if !ok || (p.Role != RoleAdmin && u.VolunteerName != p.Name) {
		h.json(w, http.StatusNotFound, map[string]string{"error": "no such card"})
		return
	}
	u, _ = h.store.RecordProgress(u.Reference, body.FilesUploaded, body.BytesUploaded, body.Status)
	h.json(w, http.StatusOK, map[string]any{"upload": u})
}

// --- admin ---

func (h *handlers) listAllUploads(w http.ResponseWriter, r *http.Request, _ Person) {
	h.json(w, http.StatusOK, map[string]any{"uploads": h.store.Uploads("")})
}

func (h *handlers) listPeople(w http.ResponseWriter, r *http.Request, _ Person) {
	h.json(w, http.StatusOK, map[string]any{"people": h.store.People()})
}

func (h *handlers) addPerson(w http.ResponseWriter, r *http.Request, _ Person) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &body); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body.Email = strings.TrimSpace(body.Email)
	if !strings.Contains(body.Email, "@") {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "a valid email address is required"})
		return
	}
	if body.Role != RoleAdmin {
		body.Role = RoleVolunteer
	}
	if _, exists := h.store.PersonByEmail(body.Email); exists {
		h.json(w, http.StatusConflict, map[string]string{"error": "that address is already on the roster"})
		return
	}
	h.json(w, http.StatusCreated, map[string]any{"person": h.store.AddPerson(strings.TrimSpace(body.Name), body.Email, body.Role)})
}

func (h *handlers) addStation(w http.ResponseWriter, r *http.Request, _ Person) {
	var body struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	}
	if err := decode(r, &body); err != nil {
		h.json(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "a station name is required"})
		return
	}
	if body.Latitude == 0 && body.Longitude == 0 {
		h.json(w, http.StatusBadRequest, map[string]string{"error": "place the recorder on the map first"})
		return
	}
	station, err := h.store.AddStation(strings.TrimSpace(body.ID), body.Name, body.Latitude, body.Longitude)
	if err != nil {
		h.json(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	h.json(w, http.StatusCreated, map[string]any{"station": station})
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

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error("encode response", "err", err)
	}
}
