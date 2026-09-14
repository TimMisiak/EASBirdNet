package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// The API and the frontend register overlapping patterns on one mux, and a bad
// combination panics inside http.ServeMux at startup rather than failing a
// package test. Wire them together here so that's caught by `go test ./...`.
func TestRoutesCoexist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>birdsense</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mux := newMux(config{StaticDir: dir}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/health", http.StatusOK},
		{"/api/v1/public/overview", http.StatusOK},
		{"/api/v1/uploads", http.StatusUnauthorized},
		{"/api/v1/nope", http.StatusNotFound},
		{"/", http.StatusOK},
		{"/stations/mercer-slough", http.StatusOK},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestConfigFromEnvDatabase(t *testing.T) {
	for _, key := range []string{"BIRDSENSE_DB", "BIRDSENSE_LOCAL_DB_PATH", "BIRDSENSE_COSMOS_ENDPOINT", "BIRDSENSE_COSMOS_DATABASE", "BIRDSENSE_COSMOS_KEY"} {
		t.Setenv(key, "")
	}

	// Cosmos is the default, and it won't start without an endpoint.
	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "BIRDSENSE_COSMOS_ENDPOINT") {
		t.Errorf("default config err = %v, want it to ask for BIRDSENSE_COSMOS_ENDPOINT", err)
	}
	t.Setenv("BIRDSENSE_COSMOS_ENDPOINT", "https://birdsense.documents.azure.com:443/")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("cosmos config: %v", err)
	}
	if cfg.DB.Backend != db.BackendCosmos || cfg.DB.CosmosDatabase != "birdsense" {
		t.Errorf("db config = %+v, want cosmos with the birdsense database", cfg.DB)
	}
	if cfg.Dev {
		t.Error("dev mode is on with the cosmos database")
	}

	t.Setenv("BIRDSENSE_DB", "local")
	t.Setenv("BIRDSENSE_LOCAL_DB_PATH", "/tmp/dev.json")
	if cfg, err = configFromEnv(); err != nil || cfg.DB.Backend != db.BackendLocal || cfg.DB.LocalPath != "/tmp/dev.json" {
		t.Errorf("local config = %+v, %v; want local at /tmp/dev.json", cfg.DB, err)
	}
	if !cfg.Dev {
		t.Error("dev mode is off with the local database")
	}

	t.Setenv("BIRDSENSE_DB", "sqlite")
	if _, err := configFromEnv(); err == nil {
		t.Error("an unknown BIRDSENSE_DB was accepted")
	}
}
