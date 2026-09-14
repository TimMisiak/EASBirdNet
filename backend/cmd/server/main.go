// Command server runs the Birdsense HTTP server: the JSON API plus the
// static frontend, so the whole app ships as one binary in one container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/api"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
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
	return cfg, nil
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
