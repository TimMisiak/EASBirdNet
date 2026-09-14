// Command server runs the Birdsense HTTP server: the JSON API plus the
// static frontend, so the whole app ships as one binary in one container.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/api"
	"github.com/ngaitonde/EASBirdNet/backend/internal/cosmos"
	"github.com/ngaitonde/EASBirdNet/backend/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg := configFromEnv()
	log.Info("starting birdsense", "addr", cfg.Addr, "static_dir", cfg.StaticDir)

	deps := database(cfg.Cosmos, log)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           requestLogger(log, newMux(cfg, log, deps...)),
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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
}

// newMux wires the two route owners together: the API claims /api/, the
// frontend takes everything else. Registration order doesn't matter, but the
// patterns do -- see web.Register.
func newMux(cfg config, log *slog.Logger, deps ...api.Dependency) *http.ServeMux {
	mux := http.NewServeMux()
	api.Register(mux, log, deps...)
	web.Register(mux, cfg.StaticDir, log)
	return mux
}

// database builds the Cosmos DB handle, if one is configured, and says in the
// log whether the account actually answers. Configuration that cannot work is
// fatal -- a wrong endpoint or key is an operator mistake worth failing on --
// but an account that is merely unreachable is not: the frontend, the public
// page and the placeholder store all work without it, and the emulator may
// still be coming up. /api/v1/health reports the live state either way.
func database(cfg cosmos.Config, log *slog.Logger) []api.Dependency {
	if !cfg.Configured() {
		log.Info("no cosmos endpoint configured; serving the in-memory placeholder store")
		return nil
	}
	client, err := cosmos.New(cfg)
	if err != nil {
		log.Error("cosmos configuration", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Check(ctx); err != nil {
		log.Warn("cosmos unreachable", "endpoint", client.Endpoint(), "err", err)
	} else {
		log.Info("cosmos reachable", "endpoint", client.Endpoint(), "database", client.Database())
	}
	return []api.Dependency{client}
}

type config struct {
	Addr      string
	StaticDir string
	Cosmos    cosmos.Config
}

func configFromEnv() config {
	return config{
		Addr:      envOr("BIRDSENSE_ADDR", ":8080"),
		StaticDir: envOr("BIRDSENSE_STATIC_DIR", "frontend"),
		Cosmos: cosmos.Config{
			Endpoint: os.Getenv("BIRDSENSE_COSMOS_ENDPOINT"),
			Key:      os.Getenv("BIRDSENSE_COSMOS_KEY"),
			Database: envOr("BIRDSENSE_COSMOS_DATABASE", "birdsense"),
		},
	}
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
