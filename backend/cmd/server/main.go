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
	"github.com/ngaitonde/EASBirdNet/backend/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg := configFromEnv()
	log.Info("starting birdsense", "addr", cfg.Addr, "static_dir", cfg.StaticDir)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           requestLogger(log, newMux(cfg, log)),
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
func newMux(cfg config, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	api.Register(mux, log)
	web.Register(mux, cfg.StaticDir, log)
	return mux
}

type config struct {
	Addr      string
	StaticDir string
}

func configFromEnv() config {
	return config{
		Addr:      envOr("BIRDSENSE_ADDR", ":8080"),
		StaticDir: envOr("BIRDSENSE_STATIC_DIR", "frontend"),
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
