// Command analyze runs BirdNET over audio files and prints the result as JSON.
// It exercises internal/birdnet by itself, before the server calls it. In the
// container:
//
//	docker compose run --rm -v "$PWD/test:/test:ro" --entrypoint /app/birdsense-analyze \
//	  birdsense "/test/2026-09-09 Osprey.wav"
//
// From backend/ in development, with a Python that has analyzer/requirements.txt:
//
//	BIRDSENSE_BIRDNET_PYTHON=../.venv/bin/python go run ./cmd/analyze "../test/2026-09-09 Osprey.wav"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
)

func main() {
	a := birdnet.Analyzer{Stderr: os.Stderr}
	var opts birdnet.Options
	var lat, lon, date string
	flag.StringVar(&a.Python, "python", envOr("BIRDSENSE_BIRDNET_PYTHON", "python3"), "Python with the birdnet package (env BIRDSENSE_BIRDNET_PYTHON)")
	flag.StringVar(&a.Script, "script", envOr("BIRDSENSE_BIRDNET_SCRIPT", "../analyzer/analyze.py"), "path to analyze.py (env BIRDSENSE_BIRDNET_SCRIPT)")
	flag.Float64Var(&opts.MinConfidence, "min-confidence", birdnet.DefaultMinConfidence, "drop detections below this, 0-1")
	flag.Float64Var(&opts.Sensitivity, "sensitivity", birdnet.DefaultSensitivity, "0.5-1.5, higher reports more")
	flag.Float64Var(&opts.OverlapSec, "overlap", 0, "seconds consecutive 3 s windows overlap")
	flag.IntVar(&opts.TopK, "top-k", birdnet.DefaultTopK, "most species per window")
	flag.IntVar(&opts.Workers, "workers", birdnet.DefaultWorkers, "inference processes")
	flag.StringVar(&lat, "lat", "", "latitude, to keep only species expected there")
	flag.StringVar(&lon, "lon", "", "longitude, with -lat")
	flag.StringVar(&date, "date", "", "YYYY-MM-DD recorded, with -lat and -lon, to narrow to that week")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] FILE...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if lat != "" || lon != "" || date != "" {
		var loc birdnet.Location
		var err1, err2 error
		loc.Latitude, err1 = strconv.ParseFloat(lat, 64)
		loc.Longitude, err2 = strconv.ParseFloat(lon, 64)
		if err1 != nil || err2 != nil {
			fail("-lat and -lon must both be numbers")
		}
		if date != "" {
			d, err := time.Parse(time.DateOnly, date)
			if err != nil {
				fail("-date must be YYYY-MM-DD")
			}
			loc.Week = birdnet.Week(d)
		}
		opts.Location = &loc
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := a.Analyze(ctx, flag.Args(), opts)
	if err != nil {
		fail(err.Error())
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "analyze:", msg)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
