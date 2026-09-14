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
	"strings"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/api"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/devseed"
	"github.com/ngaitonde/EASBirdNet/backend/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := configFromEnv()
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(1)
	}
	log.Info("starting birdsense", "addr", cfg.Addr, "static_dir", cfg.StaticDir, "db", cfg.DB.Backend, "dev", cfg.Dev)

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

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 30*time.Second)
	err = prepareDatabase(bootCtx, cfg, store, log)
	cancelBoot()
	if err != nil {
		log.Error("preparing the database", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           requestLogger(log, newMux(cfg, store, log)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Shut down cleanly so `docker compose down` doesn't cut live requests.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if err := store.Close(); err != nil {
		log.Error("closing the database", "err", err)
	}
	if shutdownErr != nil {
		log.Error("shutdown failed", "err", shutdownErr)
		os.Exit(1)
	}
}

// newMux wires the two route owners together: the API claims /api/, the
// frontend takes everything else. Registration order doesn't matter, but the
// patterns do -- see web.Register.
func newMux(cfg config, store db.Store, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	api.Register(mux, store, log, cfg.Dev)
	web.Register(mux, cfg.StaticDir, log)
	return mux
}

type config struct {
	Addr      string
	StaticDir string
	DB        db.Config
	// Dev turns on development-only affordances, like signing in as anyone on
	// the roster. It follows BIRDSENSE_DB=local: the JSON file is only ever a
	// development database, and Azure runs Cosmos, so it can't be on there.
	Dev bool
	// BootstrapAdmin, when set, is added as an admin if the roster is empty,
	// so a fresh deployment has someone who can add everyone else.
	BootstrapAdmin *mail.Address
}

// configFromEnv reads the BIRDSENSE_* variables. The database defaults to Cosmos
// DB, which is what runs in Azure; local development sets BIRDSENSE_DB=local.
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
		log.Info("seeded an empty dev database with placeholder people, recorders and cards", "path", cfg.DB.LocalPath)
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
