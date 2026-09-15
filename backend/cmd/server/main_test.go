package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/devseed"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

const testBlobEndpoint = "https://stbirdsense.blob.core.windows.net"

func testFiles(t *testing.T) storage.Store {
	t.Helper()
	files, err := storage.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return files
}

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
	mux := newMux(config{StaticDir: dir}, store, testFiles(t), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/health", http.StatusOK},
		{"/api/v1/public/overview", http.StatusOK},
		{"/api/v1/uploads", http.StatusUnauthorized},
		{"/api/v1/nope", http.StatusNotFound},
		// tusd owns every method under its path, behind the session check.
		{"/api/v1/tus/", http.StatusUnauthorized},
		{"/api/v1/tus/OWL-20260907-SR02/abc", http.StatusUnauthorized},
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
	for _, key := range []string{"BIRDSENSE_DB", "BIRDSENSE_LOCAL_DB_PATH", "BIRDSENSE_COSMOS_ENDPOINT", "BIRDSENSE_COSMOS_DATABASE", "BIRDSENSE_COSMOS_KEY", "BIRDSENSE_BOOTSTRAP_ADMIN", "BIRDSENSE_STORAGE", "BIRDSENSE_STORAGE_DIR", "BIRDSENSE_BLOB_ENDPOINT", "BIRDSENSE_BLOB_CONTAINER"} {
		t.Setenv(key, "")
	}
	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", testBlobEndpoint)

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

func TestConfigFromEnvBootstrapAdmin(t *testing.T) {
	t.Setenv("BIRDSENSE_DB", "")
	t.Setenv("BIRDSENSE_COSMOS_ENDPOINT", "https://birdsense.documents.azure.com:443/")
	t.Setenv("BIRDSENSE_STORAGE", "")
	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", testBlobEndpoint)

	t.Setenv("BIRDSENSE_BOOTSTRAP_ADMIN", "")
	if cfg, err := configFromEnv(); err != nil || cfg.BootstrapAdmin != nil {
		t.Errorf("unset bootstrap admin = %v, %v; want none", cfg.BootstrapAdmin, err)
	}

	for _, v := range []string{"Ada Admin <ada@eastsideaudubon.org>", "ada@eastsideaudubon.org"} {
		t.Setenv("BIRDSENSE_BOOTSTRAP_ADMIN", v)
		cfg, err := configFromEnv()
		if err != nil || cfg.BootstrapAdmin == nil || cfg.BootstrapAdmin.Address != "ada@eastsideaudubon.org" {
			t.Errorf("bootstrap admin %q = %v, %v; want ada@eastsideaudubon.org", v, cfg.BootstrapAdmin, err)
		}
	}

	t.Setenv("BIRDSENSE_BOOTSTRAP_ADMIN", "not an address")
	if _, err := configFromEnv(); err == nil {
		t.Error("a malformed bootstrap admin was accepted")
	}

	// Placeholder people are refused for a real database, by address or name.
	placeholders := []string{
		"Dana Coordinator <dana@eastsideaudubon.org>",
		"DANA@eastsideaudubon.org",
		"Ellen Park <ellen@eastsideaudubon.org>",
		"Real Person <real@example.com>",
		"Real Person <real@birdsense.test>",
	}
	for _, v := range placeholders {
		t.Setenv("BIRDSENSE_BOOTSTRAP_ADMIN", v)
		if _, err := configFromEnv(); err == nil {
			t.Errorf("placeholder bootstrap admin %q was accepted for cosmos", v)
		}
	}

	// Dev mode may reuse them.
	t.Setenv("BIRDSENSE_DB", "local")
	t.Setenv("BIRDSENSE_BOOTSTRAP_ADMIN", placeholders[0])
	if _, err := configFromEnv(); err != nil {
		t.Errorf("placeholder bootstrap admin in dev mode: %v", err)
	}
}

// Card audio goes where the database does unless told otherwise: Azure beside
// Cosmos, a directory in dev mode.
func TestConfigFromEnvStorage(t *testing.T) {
	for _, key := range []string{"BIRDSENSE_DB", "BIRDSENSE_LOCAL_DB_PATH", "BIRDSENSE_BOOTSTRAP_ADMIN", "BIRDSENSE_STORAGE", "BIRDSENSE_STORAGE_DIR", "BIRDSENSE_BLOB_ENDPOINT", "BIRDSENSE_BLOB_CONTAINER"} {
		t.Setenv(key, "")
	}
	t.Setenv("BIRDSENSE_COSMOS_ENDPOINT", "https://birdsense.documents.azure.com:443/")

	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "BIRDSENSE_BLOB_ENDPOINT") {
		t.Errorf("cosmos without a blob endpoint: err = %v, want it to ask for BIRDSENSE_BLOB_ENDPOINT", err)
	}
	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", testBlobEndpoint)
	cfg, err := configFromEnv()
	if err != nil || cfg.Storage.Backend != storage.BackendAzure || cfg.Storage.AzureContainer != "audio" {
		t.Errorf("cosmos storage = %+v, %v; want azure, container audio", cfg.Storage, err)
	}

	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", "")
	t.Setenv("BIRDSENSE_DB", "local")
	cfg, err = configFromEnv()
	if err != nil || cfg.Storage.Backend != storage.BackendLocal || cfg.Storage.LocalDir != "data/audio" {
		t.Errorf("dev storage = %+v, %v; want local at data/audio", cfg.Storage, err)
	}
	t.Setenv("BIRDSENSE_STORAGE", "azure")
	if _, err := configFromEnv(); err == nil {
		t.Error("azure storage without an endpoint was accepted in dev mode")
	}
	t.Setenv("BIRDSENSE_STORAGE", "s3")
	if _, err := configFromEnv(); err == nil {
		t.Error("an unknown BIRDSENSE_STORAGE was accepted")
	}
}

// Nothing the production startup path does -- bootstrapping the roster, then
// wiring the API -- may put a placeholder person into the database.
func TestProductionStartupAddsNoPlaceholderPeople(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config{
		StaticDir:      t.TempDir(),
		DB:             db.Config{Backend: db.BackendCosmos},
		BootstrapAdmin: &mail.Address{Name: "Ada Admin", Address: "ada@eastsideaudubon.org"},
	}
	for range 2 { // a restart with the setting still in place
		if err := prepareDatabase(ctx, cfg, store, log); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		mux := newMux(cfg, store, testFiles(t), nil, log)
		for _, path := range []string{"/api/v1/health", "/api/v1/session", "/api/v1/public/overview"} {
			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		}
	}

	if recorders, _ := store.ListRecorders(ctx); len(recorders) != 0 {
		t.Errorf("production startup wrote %d recorders", len(recorders))
	}
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Email != "ada@eastsideaudubon.org" || users[0].Role != db.RoleAdmin {
		t.Errorf("roster after startup = %+v, want only the bootstrap admin", users)
	}
	for _, u := range users {
		if devseed.IsPlaceholderPerson(u.Name, u.Email) {
			t.Errorf("placeholder person %s <%s> was written to the database", u.Name, u.Email)
		}
	}
}

// A fresh dev database is seeded, so the sign-in picker and the upload form
// have people and recorders on them -- unless a bootstrap admin got there
// first, which is how to start dev from a clean roster. Cards only come from
// real uploads.
func TestDevStartupSeedsAnEmptyDatabase(t *testing.T) {
	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() db.Store {
		store, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
	cfg := config{StaticDir: t.TempDir(), DB: db.Config{Backend: db.BackendLocal}, Dev: true}

	store := open()
	for range 2 {
		if err := prepareDatabase(ctx, cfg, store, log); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	mux := newMux(cfg, store, testFiles(t), nil, log)
	get := func(path string, cookie *http.Cookie) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(`{"role":"volunteer"}`)))
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("sign in as a volunteer = %d (%s)", rec.Code, rec.Body)
	}
	cookie := rec.Result().Cookies()[0]
	if body := get("/api/v1/stations", cookie); !strings.Contains(body, `"SW-01"`) {
		t.Errorf("stations after seeding = %s; want the placeholder recorders", body)
	}
	if body := get("/api/v1/uploads", cookie); strings.Contains(body, `"reference"`) {
		t.Errorf("volunteer's cards = %s; want none seeded", body)
	}
	if users, _ := store.ListUsers(ctx); len(users) != 6 {
		t.Errorf("starting twice left %d people, want the 6 seeded once", len(users))
	}

	store = open()
	cfg.BootstrapAdmin = &mail.Address{Name: "Ada Admin", Address: "ada@eastsideaudubon.org"}
	if err := prepareDatabase(ctx, cfg, store, log); err != nil {
		t.Fatalf("prepare with a bootstrap admin: %v", err)
	}
	if users, _ := store.ListUsers(ctx); len(users) != 1 {
		t.Errorf("dev with a bootstrap admin has %d people, want only the admin", len(users))
	}
}
