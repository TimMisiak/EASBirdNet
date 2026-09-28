package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/analysis"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// defaultWorkerIdle is how long a worker waits with nothing to claim before
// it exits: long enough for the next file of a card still landing, short
// enough that an idle replica isn't billed for long.
const defaultWorkerIdle = 2 * time.Minute

// runWorker is `birdsense worker`: one analysis worker, which is what each
// execution of the analysis job runs (ANALYSIS.md, *The job*). It reads the
// same environment as the server, but serves nothing, so it needs none of
// the sign-in settings. It works through the queue until nothing has been
// claimable for BIRDSENSE_ANALYSIS_IDLE (default two minutes), then exits 0;
// it exits 1 if the models can't run or the store stays unreachable, which is
// a failed execution in Azure. The status it last had is published for the
// web app to show.
func runWorker(log *slog.Logger) int {
	cfg, err := readConfig(true)
	if err != nil {
		log.Error("bad configuration", "err", err)
		return 1
	}
	idle := defaultWorkerIdle
	if v := strings.TrimSpace(os.Getenv("BIRDSENSE_ANALYSIS_IDLE")); v != "" {
		if idle, err = time.ParseDuration(v); err != nil || idle < 0 {
			log.Error("BIRDSENSE_ANALYSIS_IDLE must be a duration, like 2m", "value", v)
			return 1
		}
	}
	log.Info("analysis worker starting", "db", cfg.DB.Backend, "storage", cfg.Storage.Backend, "perch", cfg.Perch, "idle", idle)

	openCtx, cancelOpen := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := db.Open(openCtx, cfg.DB)
	cancelOpen()
	if err != nil {
		log.Error("opening the database", "err", err)
		return 1
	}
	defer store.Close()
	files, err := storage.Open(cfg.Storage)
	if err != nil {
		log.Error("opening file storage", "err", err)
		return 1
	}

	// Stopped -- the job's replica timeout, or an execution stopped by hand
	// -- the queue lets go of the files it holds before it returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	queue := newQueue(cfg, store, files, log)
	queue.OnStatus = analysis.PublishStatus(files, log)
	perfCtx, stopPerf := context.WithCancel(context.Background())
	perfDone := make(chan struct{})
	startPerf(perfCtx, perfDone, cfg, queue, files, log, map[string]any{"worker": true})
	defer func() {
		stopPerf()
		<-perfDone
	}()

	switch err := queue.RunUntilIdle(ctx, idle); {
	case err == nil:
		log.Info("analysis worker done")
		return 0
	case errors.Is(err, analysis.ErrUnavailable):
		return 1
	default:
		log.Error("analysis worker giving up", "err", err)
		return 1
	}
}
