package birdnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

// Clip is a stretch of a recording, written to its own file.
type Clip struct {
	// Path is where the clip is written, as a mono 16-bit WAV.
	Path string `json:"path"`
	// StartSec and EndSec are seconds into the source recording.
	StartSec float64 `json:"startSec"`
	EndSec   float64 `json:"endSec"`
}

// Recording is what Cut learned about a source file, and the clips it cut.
type Recording struct {
	DurationSec float64 `json:"durationSec"`
	SampleRate  int     `json:"sampleRate"`
	// Clips are in the order asked for, with StartSec and EndSec clamped to
	// the recording.
	Clips []Clip `json:"clips"`
}

// clipScript is analyzer/clip.py: ClipScript, or clip.py beside Script.
func (a Analyzer) clipScript() string {
	if a.ClipScript != "" {
		return a.ClipScript
	}
	return filepath.Join(filepath.Dir(a.Script), "clip.py")
}

// Cut writes each clip of source to its Path, by running analyzer/clip.py, and
// reports the source's duration and sample rate. With no clips it only reads
// those. It loads no model, so it takes about as long as Python takes to start.
func (a Analyzer) Cut(ctx context.Context, source string, clips []Clip) (Recording, error) {
	script := a.clipScript()
	if clips == nil {
		clips = []Clip{}
	}
	spec, err := json.Marshal(map[string]any{"source": source, "clips": clips})
	if err != nil {
		return Recording{}, err
	}

	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 4 << 10}
	cmd := exec.CommandContext(ctx, a.Python, script)
	cmd.Stdin = bytes.NewReader(spec)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if a.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, a.Stderr)
	}
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Recording{}, fmt.Errorf("birdnet: %w", ctxErr)
		}
		return Recording{}, fmt.Errorf("birdnet: %s: %w\n%s", script, err, stderr.String())
	}
	var rec Recording
	if err := json.Unmarshal(stdout.Bytes(), &rec); err != nil {
		return Recording{}, fmt.Errorf("birdnet: reading %s output: %w\n%s", script, err, stderr.String())
	}
	if len(rec.Clips) != len(clips) {
		return Recording{}, fmt.Errorf("birdnet: %s cut %d clips, not the %d asked for", script, len(rec.Clips), len(clips))
	}
	if rec.SampleRate <= 0 {
		return Recording{}, errors.New("birdnet: " + script + " reported no sample rate")
	}
	return rec, nil
}
