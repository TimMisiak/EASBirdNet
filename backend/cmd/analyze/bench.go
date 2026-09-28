package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/perf"
)

// runBench runs the model over files once per combination of workers and
// threads, repeat times each, one run at a time, and writes internal/perf's
// records to out. Each run is one Task record; the samples in between show
// its shape. A line per run goes to stderr as well.
func runBench(ctx context.Context, a birdnet.Analyzer, opts birdnet.Options, files []string, workers, threads ints, repeat int, interval time.Duration, out string) error {
	if repeat < 1 {
		return fmt.Errorf("-repeat must be at least 1")
	}
	// The files' length, read once and outside any measured run: it is what
	// the runs are divided by.
	var audioSec float64
	var size int64
	for _, f := range files {
		rec, err := a.Cut(ctx, f, nil)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}
		audioSec += rec.DurationSec
		if fi, err := os.Stat(f); err == nil {
			size += fi.Size()
		}
	}

	w, err := os.Create(out)
	if err != nil {
		return err
	}
	defer w.Close()
	// The script's own progress would bury the summary lines.
	a.Stderr = nil
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := perf.New(perf.NewWriterSink(w), perf.Config{
		Instance: perf.Instance(),
		Settings: map[string]any{"bench": true, "model": opts.Model, "files": len(files), "audioSec": audioSec},
		Interval: interval,
	}, log)
	rctx, stopRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		rec.Run(rctx)
	}()
	defer func() {
		stopRec()
		<-recDone
	}()

	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Base(f)
	}
	fmt.Fprintf(os.Stderr, "%s over %.0f s of audio in %d file(s); records to %s\n", opts.Model, audioSec, len(files), out)
	for _, wk := range workers {
		for _, th := range threads {
			for i := 0; i < repeat; i++ {
				o := opts
				o.Workers, o.Threads = wk, th
				task := rec.Begin(o.Model)
				task.Phase("analyze")
				res, err := a.Analyze(birdnet.WithObserver(ctx, task), files, o)
				t := perf.Task{
					Path: strings.Join(names, ", "), Workers: wk, Threads: th,
					Result: "ok", AudioSec: audioSec, Bytes: size,
				}
				for _, f := range res.Files {
					t.Detections += len(f.Detections)
				}
				if err != nil {
					t.Result, t.Error = "error", err.Error()
				}
				t = task.End(t)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Fprintln(os.Stderr, benchLine(t))
			}
		}
	}
	return nil
}

// benchLine is one run, for the terminal.
func benchLine(t perf.Task) string {
	head := fmt.Sprintf("workers=%d threads=%d", t.Workers, t.Threads)
	if t.Result != "ok" {
		return head + ": failed: " + t.Error
	}
	var perCPU string
	if t.CPUSec > 0 {
		perCPU = fmt.Sprintf(", %.1f s audio per CPU-second", t.AudioSec/t.CPUSec)
	}
	return fmt.Sprintf("%s: wall %.1f s (%.0fx real time), CPU %.1f s = %.2f cores%s, peak PSS %s, max RSS %s",
		head, t.WallSec, t.AudioSec/t.WallSec, t.CPUSec, t.CPUSec/t.WallSec, perCPU, mb(t.PeakPSS), mb(t.MaxRSS))
}

func mb(b int64) string { return strconv.FormatInt(b>>20, 10) + " MB" }

// ints is a flag of comma-separated integers.
type ints []int

func (v *ints) String() string {
	s := make([]string, len(*v))
	for i, n := range *v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func (v *ints) Set(s string) error {
	var out ints
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return fmt.Errorf("%q is not a list of whole numbers", s)
		}
		out = append(out, n)
	}
	*v = out
	return nil
}
