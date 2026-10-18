package birdnet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakePython writes a shell script to stand in for the interpreter. It is run
// as `fake analyze.py FLAGS... -- FILES...`, like the real one.
func fakePython(t *testing.T, body string) Analyzer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake interpreter is a shell script")
	}
	path := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return Analyzer{Python: path, Script: "analyze.py"}
}

func TestAnalyzeMapsResultsToGivenPaths(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	// Report the first file with a detection and every other one as unreadable,
	// by the absolute path the script is handed.
	a := fakePython(t, `
echo "$@" > '`+argsFile+`'
while [ "$1" != "--" ]; do shift; done; shift
printf '{"model":"BirdNET_GLOBAL_6K_V2.4","files":[{"path":"%s","detections":[{"startSec":3,"endSec":6,"scientificName":"Strix varia","commonName":"Barred Owl","confidence":0.91}]}' "$1"
shift
for f in "$@"; do printf ',{"path":"%s","error":"unreadable audio"}' "$f"; done
echo ']}'
`)
	t.Chdir(dir)

	res, err := a.Analyze(context.Background(), []string{"night1/a.wav", filepath.Join(dir, "b.wav"), "night1/a.wav"}, Options{
		Location: &Location{Latitude: 47.66, Longitude: -122.11, Week: 34},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "BirdNET_GLOBAL_6K_V2.4" {
		t.Errorf("model = %q", res.Model)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2 (duplicate dropped): %+v", len(res.Files), res.Files)
	}
	if f := res.Files[0]; f.Path != "night1/a.wav" || f.Error != "" || len(f.Detections) != 1 || f.Detections[0].CommonName != "Barred Owl" {
		t.Errorf("first file = %+v", f)
	}
	if f := res.Files[1]; f.Path != filepath.Join(dir, "b.wav") || f.Error != "unreadable audio" || f.Detections == nil {
		t.Errorf("second file = %+v (Detections must be empty, not nil)", f)
	}
	want := Options{MinConfidence: DefaultMinConfidence, Sensitivity: DefaultSensitivity, TopK: DefaultTopK, Workers: DefaultWorkers,
		Location: &Location{Latitude: 47.66, Longitude: -122.11, Week: 34}}
	if res.Options.MinConfidence != want.MinConfidence || res.Options.TopK != want.TopK || res.Options.Workers != want.Workers {
		t.Errorf("options = %+v, want defaults filled in", res.Options)
	}

	got, _ := os.ReadFile(argsFile)
	wantArgs := "analyze.py --min-confidence 0.25 --sensitivity 1 --overlap 0 --top-k 5 --workers 1 --latitude 47.66 --longitude -122.11 --week 34 -- " +
		filepath.Join(dir, "night1/a.wav") + " " + filepath.Join(dir, "b.wav")
	if strings.TrimSpace(string(got)) != wantArgs {
		t.Errorf("args:\n got  %s\n want %s", got, wantArgs)
	}
}

func TestAnalyzeFailures(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"script exits non-zero", `echo "Traceback: model file is corrupt" >&2; exit 1`, "model file is corrupt"},
		{"output isn't JSON", `echo "Downloading model..."`, "reading analyze.py output"},
		{"a file is left out", `echo '{"model":"m","files":[]}'`, "reported nothing for"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fakePython(t, c.body).Analyze(context.Background(), []string{"a.wav"}, Options{})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestAnalyzeRejectsBadOptionsWithoutRunning(t *testing.T) {
	a := Analyzer{Python: "/nonexistent/python", Script: "analyze.py"}
	for _, o := range []Options{
		{MinConfidence: 1.5},
		{Sensitivity: 3},
		{OverlapSec: 3},
		{Workers: -1},
		{Threads: -1},
		{Location: &Location{Latitude: 91}},
		{Location: &Location{Latitude: 47, Longitude: -122, Week: 49}},
		{Model: "perch-v3"},
		{Model: ModelPerch, OverlapSec: 5},
	} {
		_, err := a.Analyze(context.Background(), []string{"a.wav"}, o)
		if err == nil || errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "nonexistent") {
			t.Errorf("%+v: err = %v, want a validation error", o, err)
		}
	}
	if res, err := a.Analyze(context.Background(), nil, Options{}); err != nil || len(res.Files) != 0 {
		t.Errorf("no paths: %+v, %v; want an empty result without running", res, err)
	}
}

// Perch is asked for by name, and its 5-second windows allow a longer overlap
// than BirdNET's 3.
func TestAnalyzeAsksForPerch(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	a := fakePython(t, `
echo "$@" > '`+argsFile+`'
while [ "$1" != "--" ]; do shift; done; shift
printf '{"model":"Perch_v2","files":[{"path":"%s","detections":[]}]}' "$1"
`)
	res, err := a.Analyze(context.Background(), []string{"/cards/a.wav"}, Options{Model: ModelPerch, OverlapSec: 4})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "Perch_v2" || res.Options.Model != ModelPerch {
		t.Errorf("result = %+v", res)
	}
	got, _ := os.ReadFile(argsFile)
	want := "analyze.py --min-confidence 0.25 --sensitivity 1 --overlap 4 --top-k 5 --workers 1 --model perch -- /cards/a.wav"
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("args:\n got  %s\n want %s", got, want)
	}
}

// Threads is passed on when it is set, and an observer in the context is told
// the script's pid when it starts and its resource usage when it exits.
func TestAnalyzeThreadsAndObserver(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	a := fakePython(t, `
echo "$@" > '`+argsFile+`'
while [ "$1" != "--" ]; do shift; done; shift
printf '{"model":"Perch_v2","files":[{"path":"%s","detections":[]}]}' "$1"
`)
	var o recordingObserver
	if _, err := a.Analyze(WithObserver(context.Background(), &o), []string{"/cards/a.wav"}, Options{Model: ModelPerch, Threads: 2}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argsFile)
	want := "analyze.py --min-confidence 0.25 --sensitivity 1 --overlap 0 --top-k 5 --workers 1 --model perch --threads 2 -- /cards/a.wav"
	if strings.TrimSpace(string(got)) != want {
		t.Errorf("args:\n got  %s\n want %s", got, want)
	}
	if o.started == 0 || o.exited != o.started || o.state == nil || !o.state.Success() {
		t.Errorf("observer saw started %d, exited %d (%v)", o.started, o.exited, o.state)
	}
}

type recordingObserver struct {
	started, exited int
	state           *os.ProcessState
}

func (o *recordingObserver) Started(pid int) { o.started = pid }
func (o *recordingObserver) Exited(pid int, state *os.ProcessState) {
	o.exited, o.state = pid, state
}

// birdnet does inference in child processes, so cancelling has to take those
// down too, not just the script.
func TestAnalyzeCancelKillsChildProcesses(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	a := fakePython(t, `sleep 60 & echo $! > '`+pidFile+`'; wait`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Analyze(ctx, []string{"a.wav"}, Options{})
		done <- err
	}()

	var pid string
	for deadline := time.Now().Add(5 * time.Second); pid == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(pidFile)
		pid = strings.TrimSpace(string(b))
	}
	if pid == "" {
		t.Fatal("fake script never started its child")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Analyze didn't return after cancel")
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if !running(pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %s is still running after cancel", pid)
		}
	}
}

// running reports whether a process exists and isn't a zombie waiting for a
// parent to reap it (in a container, PID 1 may never do that).
func running(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

func TestWeek(t *testing.T) {
	for date, want := range map[string]int{
		"2026-01-01": 1, "2026-01-07": 1, "2026-01-08": 2, "2026-01-22": 4, "2026-01-31": 4,
		"2026-02-01": 5, "2026-09-09": 34, "2026-12-31": 48,
	} {
		d, _ := time.Parse(time.DateOnly, date)
		if got := Week(d); got != want {
			t.Errorf("Week(%s) = %d, want %d", date, got, want)
		}
	}
}

// TestAnalyzeOsprey runs the real model over test/2026-09-09 Osprey.wav. It
// needs a Python with the birdnet package, named by BIRDSENSE_BIRDNET_PYTHON
// (see analyzer/requirements.txt), and is skipped without one.
func TestAnalyzeOsprey(t *testing.T) {
	python := os.Getenv("BIRDSENSE_BIRDNET_PYTHON")
	if python == "" {
		t.Skip("BIRDSENSE_BIRDNET_PYTHON is not set")
	}
	clip := filepath.Join("..", "..", "..", "test", "2026-09-09 Osprey.wav")
	corrupt := filepath.Join(t.TempDir(), "corrupt.wav")
	if err := os.WriteFile(corrupt, []byte("RIFF, but not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Analyzer{Python: python, Script: filepath.Join("..", "..", "..", "analyzer", "analyze.py")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Marymoor Park, the week the clip was recorded.
	sep9, _ := time.Parse(time.DateOnly, "2026-09-09")
	for _, loc := range []*Location{nil, {Latitude: 47.66, Longitude: -122.11, Week: Week(sep9)}} {
		res, err := a.Analyze(ctx, []string{clip, "no-such-clip.wav", corrupt}, Options{Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != "BirdNET_GLOBAL_6K_V2.4" {
			t.Errorf("model = %q", res.Model)
		}
		if len(res.Files) != 3 || res.Files[1].Error != "file not found" || res.Files[2].Error != "unreadable audio" {
			t.Fatalf("files = %+v, want the clip, a not-found error and an unreadable one", res.Files)
		}
		f := res.Files[0]
		if f.Error != "" || f.Path != clip {
			t.Fatalf("clip = %+v", f)
		}
		osprey := slices.IndexFunc(f.Detections, func(d Detection) bool {
			return d.ScientificName == "Pandion haliaetus" && d.CommonName == "Osprey" && d.Confidence >= 0.8
		})
		if osprey < 0 {
			t.Errorf("location %+v: no confident Osprey in %+v", loc, f.Detections)
		}
	}
}

// TestPerchOsprey runs the real Perch model over the same clip. It needs
// analyzer/requirements-perch.txt installed as well, so it is skipped unless
// BIRDSENSE_TEST_PERCH is set too; the model downloads on first use (~380 MB).
func TestPerchOsprey(t *testing.T) {
	python := os.Getenv("BIRDSENSE_BIRDNET_PYTHON")
	if python == "" || os.Getenv("BIRDSENSE_TEST_PERCH") == "" {
		t.Skip("BIRDSENSE_BIRDNET_PYTHON and BIRDSENSE_TEST_PERCH are not both set")
	}
	a := Analyzer{Python: python, Script: filepath.Join("..", "..", "..", "analyzer", "analyze.py")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := a.CheckPerch(ctx); err != nil {
		t.Fatal(err)
	}

	clip := filepath.Join("..", "..", "..", "test", "2026-09-09 Osprey.wav")
	sep9, _ := time.Parse(time.DateOnly, "2026-09-09")
	for _, loc := range []*Location{nil, {Latitude: 47.66, Longitude: -122.11, Week: Week(sep9)}} {
		res, err := a.Analyze(ctx, []string{clip}, Options{Model: ModelPerch, Location: loc})
		if err != nil {
			t.Fatal(err)
		}
		if res.Model != "Perch_v2" || len(res.Files) != 1 {
			t.Fatalf("result = %+v", res)
		}
		// Perch's own labels are scientific names; the common name is
		// BirdNET's. Its windows are 5 seconds, and at the default threshold
		// the Osprey is all it reports: the softmax leaves nothing else near.
		ds := res.Files[0].Detections
		for _, d := range ds {
			if d.ScientificName != "Pandion haliaetus" || d.CommonName != "Osprey" || d.EndSec-d.StartSec > 5 {
				t.Errorf("location %+v: detection %+v, want only the Osprey in 5 s windows", loc, d)
			}
		}
		if !slices.ContainsFunc(ds, func(d Detection) bool { return d.Confidence >= 0.8 }) {
			t.Errorf("location %+v: no confident Osprey in %+v", loc, ds)
		}
	}
}
