// Package api holds the JSON HTTP API. Everything it serves lives under
// /api/v1/ so the static frontend can own every other path.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Detection is one BirdNET identification from one listening station.
//
// Placeholder shape: the fields here are a guess at what the UI needs, and the
// data is hard-coded (see sampleDetections). Replace with the real BirdNET
// pipeline when it lands.
type Detection struct {
	ID             string    `json:"id"`
	CommonName     string    `json:"commonName"`
	ScientificName string    `json:"scientificName"`
	Confidence     float64   `json:"confidence"`
	Station        string    `json:"station"`
	DetectedAt     time.Time `json:"detectedAt"`
}

// Register mounts the API routes on mux.
func Register(mux *http.ServeMux, log *slog.Logger) {
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, log, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/v1/detections", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, log, http.StatusOK, map[string]any{"detections": sampleDetections()})
	})

	// Anything else under /api/ is a 404 as JSON, not as the frontend's
	// index.html -- a mistyped API path should look like an API error.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, log, http.StatusNotFound, map[string]string{"error": "not found"})
	})
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Error("encode response", "err", err)
	}
}

func sampleDetections() []Detection {
	now := time.Now().UTC().Truncate(time.Second)
	return []Detection{
		{"d1", "Spotted Towhee", "Pipilo maculatus", 0.94, "Lake Hills Greenbelt", now.Add(-4 * time.Minute)},
		{"d2", "Black-capped Chickadee", "Poecile atricapillus", 0.88, "Lake Hills Greenbelt", now.Add(-21 * time.Minute)},
		{"d3", "Pileated Woodpecker", "Dryocopus pileatus", 0.81, "Mercer Slough", now.Add(-52 * time.Minute)},
		{"d4", "Barred Owl", "Strix varia", 0.76, "Mercer Slough", now.Add(-3 * time.Hour)},
	}
}
