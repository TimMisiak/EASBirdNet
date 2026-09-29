package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/api"
	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
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
	cfg := config{StaticDir: dir, SessionKey: api.RandomSessionKey()}
	mux := newMux(cfg, store, testFiles(t), nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/health", http.StatusOK},
		{"/api/v1/ready", http.StatusOK},
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

// signInEnv is the sign-in configuration every deployed server needs, so a
// test about the database or storage doesn't fail for want of a provider.
func signInEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BIRDSENSE_PUBLIC_URL", "https://owls.eastsideaudubon.org")
	t.Setenv("BIRDSENSE_SESSION_KEY", "a test session key long enough to be one")
	t.Setenv("BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID", "00000000-0000-0000-0000-000000000000")
	t.Setenv("BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET", "a test secret")
	t.Setenv("BIRDSENSE_OIDC_MICROSOFT_TENANT", "")
	t.Setenv("BIRDSENSE_OIDC_GOOGLE_CLIENT_ID", "")
	t.Setenv("BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET", "")
}

func TestConfigFromEnvDatabase(t *testing.T) {
	signInEnv(t)
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

// The documented way to run the server is `go run ./cmd/server` from backend/,
// so the default static directory is relative to backend/, not to the module
// root or to cmd/server. Getting it wrong serves the API perfectly and 404s
// every page, which looks like a broken app rather than a wrong path.
func TestConfigFromEnvStaticDirDefaultServesTheFrontend(t *testing.T) {
	signInEnv(t)
	t.Setenv("BIRDSENSE_STATIC_DIR", "")
	t.Setenv("BIRDSENSE_DB", "local")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	// This test runs in backend/cmd/server; the default is read from backend/.
	index := filepath.Join("..", "..", cfg.StaticDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		t.Errorf("BIRDSENSE_STATIC_DIR defaults to %q, which has no index.html when the server is run from backend/: %v", cfg.StaticDir, err)
	}
}

func TestConfigFromEnvBootstrapAdmin(t *testing.T) {
	signInEnv(t)
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
	signInEnv(t)
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

// Audio retention is a whole number of days, 30 unless it is set, and 0 turns
// it off. Anything else is a startup error rather than a silent default,
// because getting it wrong throws recordings away.
func TestConfigFromEnvRetention(t *testing.T) {
	signInEnv(t)
	t.Setenv("BIRDSENSE_DB", "local")
	t.Setenv("BIRDSENSE_AUDIO_RETENTION_DAYS", "")
	cfg, err := configFromEnv()
	if err != nil || cfg.Retention.Window != 30*24*time.Hour {
		t.Errorf("default retention = %v, %v; want 30 days", cfg.Retention.Window, err)
	}
	t.Setenv("BIRDSENSE_AUDIO_RETENTION_DAYS", "7")
	if cfg, err := configFromEnv(); err != nil || cfg.Retention.Window != 7*24*time.Hour {
		t.Errorf("retention = %v, %v; want 7 days", cfg.Retention.Window, err)
	}
	t.Setenv("BIRDSENSE_AUDIO_RETENTION_DAYS", "0")
	if cfg, err := configFromEnv(); err != nil || cfg.Retention.On() {
		t.Errorf("retention of 0 days = %v, %v; want it off", cfg.Retention.Window, err)
	}
	for _, bad := range []string{"-1", "a month", "30d", "0.5"} {
		t.Setenv("BIRDSENSE_AUDIO_RETENTION_DAYS", bad)
		if _, err := configFromEnv(); err == nil {
			t.Errorf("BIRDSENSE_AUDIO_RETENTION_DAYS=%q was accepted", bad)
		}
	}
}

// Perch is off unless it is turned on, and a value that is neither on nor off
// is a startup error rather than a guess.
func TestConfigFromEnvPerch(t *testing.T) {
	signInEnv(t)
	t.Setenv("BIRDSENSE_DB", "local")
	for v, want := range map[string]bool{"": false, "off": false, "0": false, "on": true, "TRUE": true, "1": true} {
		t.Setenv("BIRDSENSE_PERCH", v)
		if cfg, err := configFromEnv(); err != nil || cfg.Perch != want {
			t.Errorf("BIRDSENSE_PERCH=%q: perch = %v, %v; want %v", v, cfg.Perch, err, want)
		}
	}
	t.Setenv("BIRDSENSE_PERCH", "yes please")
	if _, err := configFromEnv(); err == nil {
		t.Error("BIRDSENSE_PERCH=yes please was accepted")
	}
}

// Performance records are on unless they are turned off.
func TestConfigFromEnvPerf(t *testing.T) {
	signInEnv(t)
	t.Setenv("BIRDSENSE_DB", "local")
	for v, want := range map[string]bool{"": true, "off": false, "on": true} {
		t.Setenv("BIRDSENSE_PERF", v)
		if cfg, err := configFromEnv(); err != nil || cfg.Perf != want {
			t.Errorf("BIRDSENSE_PERF=%q: perf = %v, %v; want %v", v, cfg.Perf, err, want)
		}
	}
	t.Setenv("BIRDSENSE_PERF", "sometimes")
	if _, err := configFromEnv(); err == nil {
		t.Error("BIRDSENSE_PERF=sometimes was accepted")
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
		SessionKey:     api.RandomSessionKey(),
	}
	for range 2 { // a restart with the setting still in place
		if err := prepareDatabase(ctx, cfg, store, log); err != nil {
			t.Fatalf("prepare: %v", err)
		}
		mux := newMux(cfg, store, testFiles(t), nil, nil, nil, log)
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
	cfg := config{StaticDir: t.TempDir(), DB: db.Config{Backend: db.BackendLocal}, Dev: true, SessionKey: api.RandomSessionKey()}

	store := open()
	for range 2 {
		if err := prepareDatabase(ctx, cfg, store, log); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	mux := newMux(cfg, store, testFiles(t), nil, nil, nil, log)
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

// Outside dev mode there is no way in but OIDC, so a deployment that has none
// configured has to say so at startup rather than when someone tries to sign
// in. Dev mode fills in what it can instead.
func TestConfigFromEnvSignIn(t *testing.T) {
	for _, key := range []string{
		"BIRDSENSE_DB", "BIRDSENSE_BLOB_ENDPOINT", "BIRDSENSE_COSMOS_ENDPOINT",
		"BIRDSENSE_PUBLIC_URL", "BIRDSENSE_SESSION_KEY",
		"BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID", "BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET",
		"BIRDSENSE_OIDC_GOOGLE_CLIENT_ID", "BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("BIRDSENSE_COSMOS_ENDPOINT", "https://birdsense.documents.azure.com:443/")
	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", testBlobEndpoint)

	for _, want := range []string{"BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID", "BIRDSENSE_PUBLIC_URL", "BIRDSENSE_SESSION_KEY"} {
		_, err := configFromEnv()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to ask for %s", err, want)
		}
		switch want {
		case "BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID":
			t.Setenv(want, "00000000-0000-0000-0000-000000000000")
			t.Setenv("BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET", "a test secret")
		case "BIRDSENSE_PUBLIC_URL":
			t.Setenv(want, "https://owls.eastsideaudubon.org/")
		case "BIRDSENSE_SESSION_KEY":
			// A key is not enough. The cookie it signs is the identity, so a
			// passphrase short enough to guess is refused like a missing one.
			t.Setenv(want, "owls")
			_, err := configFromEnv()
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at least %d", api.MinSessionKeyLen)) {
				t.Fatalf("err = %v, want a short session key refused", err)
			}
			t.Setenv(want, strings.Repeat("k", api.MinSessionKeyLen))
		}
	}

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configured sign-in: %v", err)
	}
	// The trailing slash is trimmed, so the redirect URI has one separator.
	if cfg.Auth.PublicURL != "https://owls.eastsideaudubon.org" {
		t.Errorf("public URL = %q, want it without the trailing slash", cfg.Auth.PublicURL)
	}
	if len(cfg.Auth.Providers) != 1 || cfg.Auth.Providers[0].Name != api.ProviderMicrosoft {
		t.Errorf("providers = %+v, want just microsoft", cfg.Auth.Providers)
	}

	// A client id with no secret is a half-configured provider, not a silent
	// one.
	t.Setenv("BIRDSENSE_OIDC_GOOGLE_CLIENT_ID", "google-client-id")
	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET") {
		t.Errorf("err = %v, want it to ask for the Google secret", err)
	}
	t.Setenv("BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET", "a test secret")

	// Dev mode asks for none of it: the development sign-in is there instead.
	t.Setenv("BIRDSENSE_DB", "local")
	for _, key := range []string{
		"BIRDSENSE_PUBLIC_URL", "BIRDSENSE_SESSION_KEY",
		"BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID", "BIRDSENSE_OIDC_MICROSOFT_CLIENT_SECRET",
		"BIRDSENSE_OIDC_GOOGLE_CLIENT_ID", "BIRDSENSE_OIDC_GOOGLE_CLIENT_SECRET",
	} {
		t.Setenv(key, "")
	}
	cfg, err = configFromEnv()
	switch {
	case err != nil:
		t.Fatalf("dev config: %v", err)
	case len(cfg.Auth.Providers) != 0:
		t.Errorf("dev providers = %+v, want none", cfg.Auth.Providers)
	case len(cfg.SessionKey) < api.MinSessionKeyLen:
		t.Errorf("dev mode made a %d-byte session key, want at least %d like everywhere else", len(cfg.SessionKey), api.MinSessionKeyLen)
	case cfg.Auth.PublicURL != "http://localhost:8080":
		t.Errorf("dev public URL = %q, want localhost", cfg.Auth.PublicURL)
	}
}

// A deploy that lands mid-upload is a normal shutdown, not a crash: the tus
// PATCH carrying a card's file won't finish inside the grace period, so it is
// cut and the process still exits 0. Getting this wrong makes every revision
// swap during an upload look like a failed container to Container Apps.
func TestShutdownCutsRequestsStillRunning(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})}
	defer close(release)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	go http.Get("http://" + ln.Addr().String() + "/api/v1/tus/OWL-20260907-SR02/abc")
	<-started

	done := make(chan error, 1)
	go func() { done <- shutdownHTTP(srv, 50*time.Millisecond, log) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown with a request still running = %v, want it treated as normal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not return: it waited for the request instead of cutting it")
	}

	// An idle server still shuts down cleanly.
	idle := &http.Server{Handler: http.NewServeMux()}
	idleLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go idle.Serve(idleLn)
	if err := shutdownHTTP(idle, shutdownGrace, log); err != nil {
		t.Errorf("shutdown of an idle server = %v, want nil", err)
	}
}

// The alert rule in infra/monitor.tf reads the queue's "BirdNET isn't
// available" line out of Log Analytics as a JSON field, so a logger that
// stopped emitting JSON would leave that alert matching nothing and nobody
// would hear that cards had stopped being analyzed. See newLogger.
func TestLoggerEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf).Warn("BirdNET isn't available", "err", "no such file")

	var line struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Err   string `json:"err"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("the server's log line is not JSON (%v), so infra/monitor.tf's query matches nothing: %s", err, buf.String())
	}
	if line.Msg != "BirdNET isn't available" {
		t.Errorf("msg = %q, want the message as its own field: that is what the alert query reads", line.Msg)
	}
	if line.Level != "WARN" || line.Err != "no such file" {
		t.Errorf("level = %q, err = %q, want the level and each attribute as their own fields", line.Level, line.Err)
	}
}

// The replica's size comes in Container Apps' spelling, and a typo is an
// error rather than a wrong figure in every performance record.
func TestReplicaSize(t *testing.T) {
	if cores, bytes, err := replicaSize("2", "4Gi"); err != nil || cores != 2 || bytes != 4<<30 {
		t.Errorf("2, 4Gi = %v, %v, %v", cores, bytes, err)
	}
	if cores, bytes, err := replicaSize("0.5", "1.0Gi"); err != nil || cores != 0.5 || bytes != 1<<30 {
		t.Errorf("0.5, 1.0Gi = %v, %v, %v", cores, bytes, err)
	}
	if cores, bytes, err := replicaSize("", ""); err != nil || cores != 0 || bytes != 0 {
		t.Errorf("unset = %v, %v, %v", cores, bytes, err)
	}
	for _, bad := range [][2]string{{"two", ""}, {"", "4GB"}, {"", "4096"}, {"-1", ""}} {
		if _, _, err := replicaSize(bad[0], bad[1]); err == nil {
			t.Errorf("%q, %q was accepted", bad[0], bad[1])
		}
	}
}

// A replica runs a task per core in its memory less the headroom; a machine
// that only knows the host runs two, so a laptop isn't taken over.
func TestAnalysisCapacity(t *testing.T) {
	tune := analysisTuning{Headroom: 1 << 30}
	if c := analysisCapacity(tune, 2, 4<<30, "config"); c.Cores != 2 || c.MaxTasks != 2 || c.Memory != 3<<30 {
		t.Errorf("2 vCPU / 4Gi replica = %+v", c)
	}
	if c := analysisCapacity(tune, 32, 64<<30, "host"); c.Cores != 2 || c.MaxTasks != 2 {
		t.Errorf("a 32-core host = %+v, want two tasks on two cores", c)
	}
	if c := analysisCapacity(tune, 0.5, 1<<30, "cgroup"); c.MaxTasks != 1 || c.Memory != 1 {
		t.Errorf("a replica smaller than the headroom = %+v, want one task at a time", c)
	}
	tune.MaxTasks = 3
	if c := analysisCapacity(tune, 2, 8<<30, "config"); c.MaxTasks != 3 {
		t.Errorf("MaxTasks set = %+v, want 3", c)
	}
}

func TestAnalysisTuning(t *testing.T) {
	signInEnv(t)
	t.Setenv("BIRDSENSE_DB", "local")
	t.Setenv("BIRDSENSE_ANALYSIS_PERCH_MEM", "2.5Gi")
	t.Setenv("BIRDSENSE_ANALYSIS_BIRDNET_MEM", "300Mi")
	t.Setenv("BIRDSENSE_ANALYSIS_PERCH_THREADS", "2")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	costs := analysisCosts(cfg.Analysis)
	if p := costs[birdnet.ModelPerch]; p.Memory != 5<<29 || p.Threads != 2 {
		t.Errorf("perch = %+v", p)
	}
	if b := costs[birdnet.ModelBirdNET]; b.Memory != 300<<20 {
		t.Errorf("birdnet = %+v", b)
	}
	if cfg.Analysis.Headroom != 1<<30 {
		t.Errorf("headroom = %d, want the 1Gi default", cfg.Analysis.Headroom)
	}
	for name, bad := range map[string]string{"BIRDSENSE_ANALYSIS_PERCH_MEM": "2.5GB", "BIRDSENSE_ANALYSIS_MAX_TASKS": "-1"} {
		t.Setenv(name, bad)
		if _, err := configFromEnv(); err == nil {
			t.Errorf("%s=%s was accepted", name, bad)
		}
		t.Setenv(name, "")
	}
}

// A worker serves nothing, so it starts on a production configuration
// without the sign-in settings the web app requires; the web app reads the
// job it starts workers of.
func TestWorkerConfigNeedsNoSignIn(t *testing.T) {
	t.Setenv("BIRDSENSE_DB", "cosmos")
	t.Setenv("BIRDSENSE_COSMOS_ENDPOINT", "https://cosmos.example.net")
	t.Setenv("BIRDSENSE_BLOB_ENDPOINT", "https://st.example.net")
	t.Setenv("BIRDSENSE_ANALYSIS_JOB", "/subscriptions/s/resourceGroups/rg/providers/Microsoft.App/jobs/caj")
	t.Setenv("BIRDSENSE_ANALYSIS_JOB_WORKERS", "4")
	if _, err := configFromEnv(); err == nil || !strings.Contains(err.Error(), "SESSION_KEY") && !strings.Contains(err.Error(), "OIDC") {
		t.Fatalf("web app config = %v, want it refused for want of sign-in settings", err)
	}
	cfg, err := readConfig(true)
	if err != nil {
		t.Fatalf("worker config: %v", err)
	}
	if cfg.AnalysisJob == "" || cfg.AnalysisWorkers != 4 {
		t.Errorf("job = %q, workers = %d", cfg.AnalysisJob, cfg.AnalysisWorkers)
	}
	t.Setenv("BIRDSENSE_ANALYSIS_JOB_WORKERS", "0")
	if _, err := readConfig(true); err == nil {
		t.Error("BIRDSENSE_ANALYSIS_JOB_WORKERS=0 was accepted")
	}
}

// The request log says what each request was answered with and how big its
// body was -- a tus PATCH's chunk -- and the writer it wraps the response in
// still lets a handler reach the connection: tusd sets read deadlines through
// http.ResponseController, and a wrapper that hid them would fail every PATCH.
func TestRequestLoggerRecordsStatusAndBytes(t *testing.T) {
	var out bytes.Buffer
	var deadlineErr error
	h := requestLogger(newLogger(&out), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineErr = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/v1/tus/x/y", strings.NewReader("chunk"))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if deadlineErr != nil {
		t.Errorf("setting a read deadline through the logged writer: %v", deadlineErr)
	}
	var line struct {
		Msg    string `json:"msg"`
		Method string `json:"method"`
		Status int    `json:"status"`
		Bytes  int64  `json:"bytes"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", out.String(), err)
	}
	if line.Msg != "request" || line.Method != http.MethodPatch || line.Status != http.StatusNoContent || line.Bytes != 5 {
		t.Errorf("logged %+v, want a PATCH answered 204 with a 5-byte body", line)
	}
}
