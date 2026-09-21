// Command server runs the Birdsense HTTP server: the JSON API plus the
// static frontend, so the whole app ships as one binary in one container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/analysis"
	"github.com/ngaitonde/EASBirdNet/backend/internal/api"
	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/devseed"
	"github.com/ngaitonde/EASBirdNet/backend/internal/retention"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
	"github.com/ngaitonde/EASBirdNet/backend/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := configFromEnv()
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}
	log.Info("starting birdsense", "addr", cfg.Addr, "static_dir", cfg.StaticDir, "db", cfg.DB.Backend, "storage", cfg.Storage.Backend, "dev", cfg.Dev)

	openCtx, cancelOpen := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := db.Open(openCtx, cfg.DB)
	cancelOpen()
	if err != nil {
		log.Error("opening the database", "err", err)
		os.Exit(1)
	}
	switch cfg.DB.Backend {
	case db.BackendLocal:
		log.Info("database ready", "backend", db.BackendLocal, "path", cfg.DB.LocalPath)
	default:
		log.Info("database ready", "backend", db.BackendCosmos,
			"endpoint", cfg.DB.CosmosEndpoint, "database", cfg.DB.CosmosDatabase, "key_auth", cfg.DB.CosmosKey != "")
	}

	files, err := storage.Open(cfg.Storage)
	if err != nil {
		log.Error("opening file storage", "err", err)
		os.Exit(1)
	}
	switch cfg.Storage.Backend {
	case storage.BackendLocal:
		log.Info("file storage ready", "backend", storage.BackendLocal, "dir", cfg.Storage.LocalDir)
	default:
		log.Info("file storage ready", "backend", storage.BackendAzure,
			"endpoint", cfg.Storage.AzureEndpoint, "container", cfg.Storage.AzureContainer)
	}

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 30*time.Second)
	err = prepareDatabase(bootCtx, cfg, store, log)
	cancelBoot()
	if err != nil {
		log.Error("preparing the database", "err", err)
		os.Exit(1)
	}

	// Shut down cleanly so `docker compose down` doesn't cut live requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// BirdNET runs in this process, over received cards. Whether it can run is
	// the queue's own business: it checks before it starts and keeps checking,
	// reports what it is waiting for through the admin API, and needs no
	// restart once the environment is right. So the server starts it either
	// way -- an unanalyzable card waits in processing rather than being lost.
	queue := analysis.New(store, files, cfg.Analyzer, log)
	analysisCtx, stopAnalysis := context.WithCancel(context.Background())
	analysisDone := make(chan struct{})
	log.Info("analysis queue starting", "python", cfg.Analyzer.Python, "script", cfg.Analyzer.Script)
	go func() {
		defer close(analysisDone)
		queue.Run(analysisCtx)
	}()

	// Audio retention: the originals of a card BirdNET has finished with go
	// after their window, and its detections and clips stay. Unlike analysis
	// this needs nothing but the store, so it runs whether or not BirdNET is
	// there -- audio a queue-less server can't analyze is audio it must keep,
	// which is exactly what the sweep's "settled cards only" rule does.
	sweeper := retention.New(store, files, cfg.Retention, log)
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	retentionDone := make(chan struct{})
	if cfg.Retention.On() {
		log.Info("audio retention started", "days", int(cfg.Retention.Window/(24*time.Hour)))
		go func() {
			defer close(retentionDone)
			sweeper.Run(retentionCtx)
		}()
	} else {
		close(retentionDone)
		log.Info("audio retention is off: card originals are kept until the card is deleted")
	}

	// Sign-in. Reaching the provider is a network call, so a provider that is
	// unreachable now is retried when someone signs in rather than stopping
	// the server -- the same call BirdNET's check makes above.
	authCtx, cancelAuth := context.WithTimeout(ctx, 30*time.Second)
	auth, err := api.NewAuthenticator(authCtx, cfg.Auth, log)
	cancelAuth()
	if err != nil {
		log.Error("bad sign-in configuration", "err", err)
		os.Exit(1)
	}
	for provider, uri := range auth.RedirectURIs() {
		log.Info("register this redirect URI with the provider", "provider", provider, "redirect_uri", uri)
	}
	if auth == nil {
		log.Warn("no identity provider is configured, so the only way in is the development sign-in")
	}

	// No ReadTimeout or WriteTimeout: a PATCH carrying audio takes as long as
	// the volunteer's upstream link needs. tusd sets deadlines per read instead.
	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: requestLogger(log, web.SecurityHeaders(
			newMux(cfg, store, files, queue, auth, log),
			// Dev is http://localhost, where HSTS is ignored anyway.
			web.SecurityOptions{StaticDir: cfg.StaticDir, HTTPS: !cfg.Dev, Log: log},
		)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownErr := shutdownHTTP(srv, shutdownGrace, log)
	// A BirdNET run in flight is killed; its file is queued again next start.
	stopAnalysis()
	<-analysisDone
	// A sweep in flight stops between files; the next start finishes it.
	stopRetention()
	<-retentionDone
	if err := store.Close(); err != nil {
		log.Error("closing the database", "err", err)
	}
	if shutdownErr != nil {
		log.Error("shutdown failed", "err", shutdownErr)
		os.Exit(1)
	}
}

// shutdownGrace is how long live requests get to finish once a signal
// arrives. It is well inside Container Apps' 30 s termination grace period,
// and many times what any ordinary request needs -- including the overview
// scan, the slowest read in the app.
const shutdownGrace = 10 * time.Second

// shutdownHTTP stops the server, then cuts whatever is still running.
//
// A card's 50 MB tus PATCH on a home connection routinely takes longer than
// shutdownGrace, so on a deploy mid-upload the deadline passing is the normal
// outcome, not a failure: waiting for the file would only push the process
// past the grace period and be killed anyway, and the browser's tus client
// resumes that file from the last chunk the server stored. So the requests
// still in flight are cut, it is logged at Warn, and the process exits 0.
// Only a real Shutdown failure is an error.
func shutdownHTTP(srv *http.Server, grace time.Duration, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	log.Warn("requests were still running at shutdown and have been cut; an upload in flight resumes from its last chunk", "grace", grace)
	if err := srv.Close(); err != nil {
		log.Warn("closing the listener", "err", err)
	}
	return nil
}

// newMux wires the two route owners together: the API claims /api/, the
// frontend takes everything else. Registration order doesn't matter, but the
// patterns do -- see web.Register.
func newMux(cfg config, store db.Store, files storage.Store, queue api.Queue, auth *api.Authenticator, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	api.Register(mux, api.Options{
		Store: store, Files: files, Queue: queue, Log: log,
		Dev: cfg.Dev, Auth: auth, SessionKey: cfg.SessionKey, Retention: cfg.Retention,
	})
	web.Register(mux, cfg.StaticDir, log)
	return mux
}

type config struct {
	Addr      string
	StaticDir string
	DB        db.Config
	// Storage is where card audio goes.
	Storage storage.Config
	// Analyzer runs BirdNET over received cards.
	Analyzer birdnet.Analyzer
	// Dev turns on development-only affordances, like signing in as anyone on
	// the roster. It follows BIRDSENSE_DB=local: the JSON file is only ever a
	// development database, and Azure runs Cosmos, so it can't be on there.
	Dev bool
	// BootstrapAdmin, when set, is added as an admin if the roster is empty,
	// so a fresh deployment has someone who can add everyone else.
	BootstrapAdmin *mail.Address
	// Auth is OIDC sign-in. Outside dev mode it is required: there is no other
	// way in.
	Auth api.AuthConfig
	// SessionKey signs the session cookie. Outside dev mode it is required, so
	// a restart or a new revision doesn't sign everyone out.
	SessionKey string
	// Retention is how long a card's original recordings are kept once BirdNET
	// has finished with them. Detections and their clips are kept for good
	// either way.
	Retention retention.Policy
}

// configFromEnv reads the BIRDSENSE_* variables. The database defaults to Cosmos
// DB, which is what runs in Azure; local development sets BIRDSENSE_DB=local.
// File storage follows the database unless BIRDSENSE_STORAGE says otherwise:
// a directory in dev mode, Azure Blob Storage everywhere else.
func configFromEnv() (config, error) {
	cfg := config{
		Addr:      envOr("BIRDSENSE_ADDR", ":8080"),
		StaticDir: envOr("BIRDSENSE_STATIC_DIR", "frontend"),
		DB: db.Config{
			Backend:        envOr("BIRDSENSE_DB", db.BackendCosmos),
			LocalPath:      envOr("BIRDSENSE_LOCAL_DB_PATH", "data/birdsense.json"),
			CosmosEndpoint: os.Getenv("BIRDSENSE_COSMOS_ENDPOINT"),
			CosmosDatabase: envOr("BIRDSENSE_COSMOS_DATABASE", "birdsense"),
			CosmosKey:      os.Getenv("BIRDSENSE_COSMOS_KEY"),
		},
		Storage: storage.Config{
			Backend:        os.Getenv("BIRDSENSE_STORAGE"),
			LocalDir:       envOr("BIRDSENSE_STORAGE_DIR", "data/audio"),
			AzureEndpoint:  os.Getenv("BIRDSENSE_BLOB_ENDPOINT"),
			AzureContainer: envOr("BIRDSENSE_BLOB_CONTAINER", "audio"),
		},
		// The defaults suit `go run ./cmd/server` from backend/ with the venv
		// from CLAUDE.md; the image sets both.
		Analyzer: birdnet.Analyzer{
			Python: envOr("BIRDSENSE_BIRDNET_PYTHON", "../.venv/bin/python"),
			Script: envOr("BIRDSENSE_BIRDNET_SCRIPT", "../analyzer/analyze.py"),
		},
	}
	switch cfg.DB.Backend {
	case db.BackendLocal:
		cfg.Dev = true
	case db.BackendCosmos:
		if cfg.DB.CosmosEndpoint == "" {
			return cfg, errors.New("BIRDSENSE_COSMOS_ENDPOINT is required for the cosmos database (for local development, set BIRDSENSE_DB=local)")
		}
	default:
		return cfg, fmt.Errorf("BIRDSENSE_DB must be %q or %q, not %q", db.BackendCosmos, db.BackendLocal, cfg.DB.Backend)
	}

	if cfg.Storage.Backend == "" {
		cfg.Storage.Backend = storage.BackendAzure
		if cfg.Dev {
			cfg.Storage.Backend = storage.BackendLocal
		}
	}
	switch cfg.Storage.Backend {
	case storage.BackendLocal:
	case storage.BackendAzure:
		if cfg.Storage.AzureEndpoint == "" {
			return cfg, errors.New("BIRDSENSE_BLOB_ENDPOINT is required for azure file storage (for local development, set BIRDSENSE_DB=local)")
		}
	default:
		return cfg, fmt.Errorf("BIRDSENSE_STORAGE must be %q or %q, not %q", storage.BackendLocal, storage.BackendAzure, cfg.Storage.Backend)
	}

	// Audio retention. Days rather than a duration, because that is how the
	// policy is written down (DEPLOYMENT.md) and how a coordinator reads it.
	// 0 keeps the originals until someone deletes the card.
	days := 30
	if v := strings.TrimSpace(os.Getenv("BIRDSENSE_AUDIO_RETENTION_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("BIRDSENSE_AUDIO_RETENTION_DAYS must be a whole number of days, 0 to keep originals for good, not %q", v)
		}
		days = n
	}
	cfg.Retention = retention.Policy{Window: time.Duration(days) * 24 * time.Hour}

	if err := readAuth(&cfg); err != nil {
		return cfg, err
	}

	if v := strings.TrimSpace(os.Getenv("BIRDSENSE_BOOTSTRAP_ADMIN")); v != "" {
		addr, err := mail.ParseAddress(v)
		if err != nil {
			return cfg, fmt.Errorf(`BIRDSENSE_BOOTSTRAP_ADMIN must be "Name <email>" or an email, not %q: %w`, v, err)
		}
		// The placeholder roster is only for exercising the frontend. Dev mode
		// may reuse it; anything else is a real database, where one of those
		// people as admin would be a typo at best and a way in at worst.
		if !cfg.Dev && (devseed.IsPlaceholderPerson(addr.Name, addr.Address) || reservedDomain(addr.Address)) {
			return cfg, fmt.Errorf("BIRDSENSE_BOOTSTRAP_ADMIN %q is development placeholder data; name a real person for the %s database", v, cfg.DB.Backend)
		}
		cfg.BootstrapAdmin = addr
	}
	return cfg, nil
}

// reservedDomain reports whether an address is on a domain RFC 2606 sets aside
// for documentation and testing, so it can never be a real mailbox.
func reservedDomain(email string) bool {
	domain := strings.ToLower(email[strings.LastIndex(email, "@")+1:])
	for _, d := range []string{"example.com", "example.net", "example.org"} {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	for _, tld := range []string{".example", ".test", ".invalid", ".localhost"} {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	return false
}

// readAuth reads the sign-in configuration. A provider is configured when its
// client id is set, so adding Google later is two more variables and no code.
//
// Outside dev mode all of it is required, because the development sign-in is
// not registered there: a deployment with no provider has no way in at all,
// which is better found at startup than by the first volunteer.
func readAuth(cfg *config) error {
	cfg.SessionKey = os.Getenv("BIRDSENSE_SESSION_KEY")
	cfg.Auth = api.AuthConfig{PublicURL: strings.TrimSuffix(os.Getenv("BIRDSENSE_PUBLIC_URL"), "/")}

	for _, p := range []struct{ name, prefix string }{
		{api.ProviderMicrosoft, "BIRDSENSE_OIDC_MICROSOFT"},
		{api.ProviderGoogle, "BIRDSENSE_OIDC_GOOGLE"},
	} {
		id := os.Getenv(p.prefix + "_CLIENT_ID")
		if id == "" {
			continue
		}
		secret := os.Getenv(p.prefix + "_CLIENT_SECRET")
		if secret == "" {
			return fmt.Errorf("%s_CLIENT_ID is set, so %s_CLIENT_SECRET is required too", p.prefix, p.prefix)
		}
		cfg.Auth.Providers = append(cfg.Auth.Providers, api.ProviderConfig{
			Name: p.name, ClientID: id, ClientSecret: secret,
			Tenant: os.Getenv(p.prefix + "_TENANT"),
		})
	}

	if cfg.Dev {
		if cfg.SessionKey == "" {
			// Dev signs everyone out on restart rather than asking for a key.
			cfg.SessionKey = api.RandomSessionKey()
		}
		if cfg.Auth.PublicURL == "" {
			cfg.Auth.PublicURL = "http://localhost:8080"
		}
		return nil
	}

	switch {
	case len(cfg.Auth.Providers) == 0:
		return errors.New("BIRDSENSE_OIDC_MICROSOFT_CLIENT_ID (or _GOOGLE_) is required: outside development mode, OpenID Connect is the only way to sign in")
	case cfg.Auth.PublicURL == "":
		return errors.New("BIRDSENSE_PUBLIC_URL is required: it is where the identity provider sends people back to, e.g. https://owls.eastsideaudubon.org")
	case cfg.SessionKey == "":
		return errors.New("BIRDSENSE_SESSION_KEY is required: it signs the session cookie, and a fresh one each start would sign everyone out")
	case len(cfg.SessionKey) < api.MinSessionKeyLen:
		return fmt.Errorf("BIRDSENSE_SESSION_KEY is %d bytes long, and at least %d are required: the cookie it signs is the identity, so a guessable key is a forged session -- generate one with `openssl rand -base64 32`", len(cfg.SessionKey), api.MinSessionKeyLen)
	}
	return nil
}

// prepareDatabase is everything the server writes on its own before it serves:
// the bootstrap admin, and in dev mode the placeholder program if the database
// is still empty after that. Outside dev mode the bootstrap admin is the only
// write the server ever makes unasked.
func prepareDatabase(ctx context.Context, cfg config, store db.Store, log *slog.Logger) error {
	if err := bootstrapRoster(ctx, cfg, store, log); err != nil {
		return fmt.Errorf("bootstrapping the roster: %w", err)
	}
	if !cfg.Dev {
		return nil
	}
	seeded, err := devseed.Seed(ctx, store, time.Now())
	if err != nil {
		return fmt.Errorf("seeding the dev database: %w", err)
	}
	if seeded {
		log.Info("seeded an empty dev database with placeholder people and recorders", "path", cfg.DB.LocalPath)
	}
	return nil
}

// bootstrapRoster adds the configured first admin to an empty roster.
func bootstrapRoster(ctx context.Context, cfg config, store db.Store, log *slog.Logger) error {
	if cfg.BootstrapAdmin == nil {
		return nil
	}
	email := cfg.BootstrapAdmin.Address
	u, created, err := db.BootstrapAdmin(ctx, store, cfg.BootstrapAdmin.Name, email)
	switch {
	case err != nil:
		return err
	case created:
		log.Info("added the bootstrap admin to an empty roster", "email", u.Email, "id", u.ID)
	case u.ID == "":
		log.Warn("BIRDSENSE_BOOTSTRAP_ADMIN is not on the roster, and the roster is not empty, so it was not added", "email", email)
	default:
		log.Info("roster already bootstrapped", "email", u.Email, "role", u.Role)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("request", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
	})
}
