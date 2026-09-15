// Package birdnet identifies birds in audio files by running BirdNET.
//
// BirdNET's maintained runtime is the `birdnet` Python package, so rather than
// link a model runtime into the Go binary, Analyze runs analyzer/analyze.py as a
// subprocess and reads its JSON. The Docker image carries the Python
// environment and the models; see the Dockerfile.
package birdnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Defaults for zero Options fields. MinConfidence and Sensitivity match
// BirdNET-Analyzer's own defaults.
const (
	DefaultMinConfidence = 0.25
	DefaultSensitivity   = 1.0
	DefaultTopK          = 5
	DefaultWorkers       = 1
)

// Analyzer runs analyze.py with a particular Python.
type Analyzer struct {
	// Python is the interpreter with the birdnet package installed, e.g. the
	// image's /opt/birdnet/venv/bin/python.
	Python string
	// Script is the path to analyzer/analyze.py.
	Script string
	// ClipScript is the path to analyzer/clip.py, which Cut runs. Empty means
	// clip.py in the same directory as Script.
	ClipScript string
	// Stderr, if set, receives the script's logs as they are written. The tail
	// of them is included in the error when a run fails either way.
	Stderr io.Writer
}

// Options tune a run. Zero fields take the Default* values, so the zero Options
// is BirdNET's usual configuration with no location filter.
type Options struct {
	// MinConfidence drops detections scored below it (0-1).
	MinConfidence float64 `json:"minConfidence"`
	// Sensitivity scales the model's sigmoid, 0.5-1.5; higher reports more.
	Sensitivity float64 `json:"sensitivity"`
	// OverlapSec is how far consecutive 3-second windows overlap, 0 to <3.
	OverlapSec float64 `json:"overlapSec"`
	// TopK is the most species reported for any one window.
	TopK int `json:"topK"`
	// Workers is the number of inference processes. Each loads its own copy
	// of the model (~250 MB resident), so raise it with the memory limit.
	Workers int `json:"workers"`
	// Location, if set, limits the species to those BirdNET's geo model
	// expects there.
	Location *Location `json:"location,omitempty"`
}

// Location is where (and optionally when) a recording was made.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// Week is BirdNET's week of the year, 1-48 (see Week); 0 means year-round.
	Week int `json:"week,omitempty"`
}

// Result is one run over a batch of files.
type Result struct {
	// Model names the model that produced the detections, e.g.
	// "BirdNET_GLOBAL_6K_V2.4", the form db.Analysis.Model stores.
	Model string `json:"model"`
	// Options are the settings the run actually used, defaults filled in.
	Options Options `json:"options"`
	// Files holds one entry per requested path, in the order given.
	Files []File `json:"files"`
}

// File is BirdNET's output for one audio file.
type File struct {
	// Path is the path as it was passed to Analyze.
	Path string `json:"path"`
	// Detections are sorted by start time, then by confidence, highest first.
	// Non-bird classes ("Engine", "Dog", "Human vocal") are included.
	Detections []Detection `json:"detections"`
	// Error is set when the file couldn't be analyzed (missing, unreadable);
	// Detections is then empty. Other files in the run are unaffected.
	Error string `json:"error,omitempty"`
}

// Detection is one species heard in one window of a file.
type Detection struct {
	StartSec       float64 `json:"startSec"`
	EndSec         float64 `json:"endSec"`
	ScientificName string  `json:"scientificName"`
	CommonName     string  `json:"commonName"`
	Confidence     float64 `json:"confidence"`
}

// Analyze runs BirdNET over paths in one process, so the model loads once for
// the whole batch. It fails as a whole only if the run itself does; a file
// that can't be read is reported in its File.Error. Cancelling ctx kills the
// script and its worker processes.
func (a Analyzer) Analyze(ctx context.Context, paths []string, opts Options) (Result, error) {
	opts = opts.withDefaults()
	if len(paths) == 0 {
		return Result{Options: opts, Files: []File{}}, nil
	}
	if err := opts.validate(); err != nil {
		return Result{}, err
	}

	// The script reports absolute paths; remember what the caller called each.
	absToGiven := make(map[string]string, len(paths))
	order := make([]string, 0, len(paths))
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return Result{}, fmt.Errorf("birdnet: %q: %w", p, err)
		}
		if _, dup := absToGiven[abs]; dup {
			continue
		}
		absToGiven[abs] = p
		order = append(order, abs)
	}
	args := append([]string{a.Script}, opts.args()...)
	args = append(args, "--")
	args = append(args, order...)

	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 4 << 10}
	cmd := exec.CommandContext(ctx, a.Python, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if a.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, a.Stderr)
	}
	killProcessGroupOnCancel(cmd)
	// Worker processes inherit the output pipes; don't wait on them forever
	// if one outlives the script.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, fmt.Errorf("birdnet: %w", ctxErr)
		}
		return Result{}, fmt.Errorf("birdnet: %s: %w\n%s", a.Script, err, stderr.String())
	}

	var out struct {
		Model string `json:"model"`
		Files []File `json:"files"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Result{}, fmt.Errorf("birdnet: reading %s output: %w\n%s", a.Script, err, stderr.String())
	}
	byPath := make(map[string]File, len(out.Files))
	for _, f := range out.Files {
		byPath[f.Path] = f
	}

	res := Result{Model: out.Model, Options: opts, Files: make([]File, 0, len(order))}
	for _, abs := range order {
		f, ok := byPath[abs]
		if !ok {
			return Result{}, fmt.Errorf("birdnet: %s reported nothing for %q", a.Script, absToGiven[abs])
		}
		f.Path = absToGiven[abs]
		if f.Detections == nil {
			f.Detections = []Detection{}
		}
		res.Files = append(res.Files, f)
	}
	return res, nil
}

// Week returns BirdNET's week of the year for a date: four weeks to a month,
// days 1-7 in the first, 8-14 the second, 15-21 the third, the rest the fourth.
func Week(t time.Time) int {
	return (int(t.Month())-1)*4 + min((t.Day()-1)/7+1, 4)
}

func (o Options) withDefaults() Options {
	if o.MinConfidence == 0 {
		o.MinConfidence = DefaultMinConfidence
	}
	if o.Sensitivity == 0 {
		o.Sensitivity = DefaultSensitivity
	}
	if o.TopK == 0 {
		o.TopK = DefaultTopK
	}
	if o.Workers == 0 {
		o.Workers = DefaultWorkers
	}
	return o
}

func (o Options) validate() error {
	switch {
	case o.MinConfidence < 0 || o.MinConfidence > 1:
		return fmt.Errorf("birdnet: MinConfidence %v is outside 0-1", o.MinConfidence)
	case o.Sensitivity < 0.5 || o.Sensitivity > 1.5:
		return fmt.Errorf("birdnet: Sensitivity %v is outside 0.5-1.5", o.Sensitivity)
	case o.OverlapSec < 0 || o.OverlapSec >= 3:
		return fmt.Errorf("birdnet: OverlapSec %v is outside 0 to <3", o.OverlapSec)
	case o.TopK < 1:
		return errors.New("birdnet: TopK must be positive")
	case o.Workers < 1:
		return errors.New("birdnet: Workers must be positive")
	}
	if l := o.Location; l != nil {
		switch {
		case l.Latitude < -90 || l.Latitude > 90 || l.Longitude < -180 || l.Longitude > 180:
			return fmt.Errorf("birdnet: location %v,%v is not a coordinate", l.Latitude, l.Longitude)
		case l.Week < 0 || l.Week > 48:
			return fmt.Errorf("birdnet: Week %d is outside 1-48 (or 0 for year-round)", l.Week)
		}
	}
	return nil
}

// args spells out every setting, so the script's own defaults never apply.
func (o Options) args() []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	args := []string{
		"--min-confidence", f(o.MinConfidence),
		"--sensitivity", f(o.Sensitivity),
		"--overlap", f(o.OverlapSec),
		"--top-k", strconv.Itoa(o.TopK),
		"--workers", strconv.Itoa(o.Workers),
	}
	if l := o.Location; l != nil {
		args = append(args, "--latitude", f(l.Latitude), "--longitude", f(l.Longitude))
		if l.Week != 0 {
			args = append(args, "--week", strconv.Itoa(l.Week))
		}
	}
	return args
}

// tailBuffer keeps the last max bytes written to it: enough of the script's
// stderr to explain a failure, without holding a whole night's progress log.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(bytes.TrimSpace(t.buf)) }

// Check reports whether Analyze and Cut can run: the scripts are there, and the
// Python can import the birdnet package. It loads no model, so it takes a second, not
// the minute a real run can.
func (a Analyzer) Check(ctx context.Context) error {
	for _, script := range []string{a.Script, a.clipScript()} {
		if _, err := os.Stat(script); err != nil {
			return fmt.Errorf("birdnet: %w", err)
		}
	}
	var stderr tailBuffer
	stderr.max = 1 << 10
	cmd := exec.CommandContext(ctx, a.Python, "-c", "import birdnet")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("birdnet: %s can't import the birdnet package: %w\n%s", a.Python, err, stderr.String())
	}
	return nil
}
