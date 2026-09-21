package birdnet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCutSendsTheClipsAndReadsTheRecording(t *testing.T) {
	dir := t.TempDir()
	argsFile, specFile := filepath.Join(dir, "args"), filepath.Join(dir, "spec")
	a := fakePython(t, `
echo "$@" > '`+argsFile+`'
cat > '`+specFile+`'
echo '{"durationSec":13.96,"sampleRate":44100,"clips":[{"path":"/tmp/a.flac","startSec":2,"endSec":5},{"path":"/tmp/b.flac","startSec":12,"endSec":13.96}]}'
`)
	a.Script = filepath.Join("analyzer", "analyze.py")

	rec, err := a.Cut(context.Background(), "/cards/x.wav", []Clip{
		{Path: "/tmp/a.flac", StartSec: 2, EndSec: 5},
		{Path: "/tmp/b.flac", StartSec: 12, EndSec: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.DurationSec != 13.96 || rec.SampleRate != 44100 || len(rec.Clips) != 2 || rec.Clips[1].EndSec != 13.96 {
		t.Errorf("recording = %+v", rec)
	}
	if got, _ := os.ReadFile(argsFile); strings.TrimSpace(string(got)) != filepath.Join("analyzer", "clip.py") {
		t.Errorf("ran %q; want clip.py beside analyze.py", got)
	}
	var spec struct {
		Source string `json:"source"`
		Clips  []Clip `json:"clips"`
	}
	got, _ := os.ReadFile(specFile)
	if err := json.Unmarshal(got, &spec); err != nil || spec.Source != "/cards/x.wav" || len(spec.Clips) != 2 || spec.Clips[1].EndSec != 20 {
		t.Errorf("spec on stdin = %s (%v)", got, err)
	}
}

func TestCutFailures(t *testing.T) {
	cases := []struct{ name, body, wantErr string }{
		{"script fails", "echo 'LibsndfileError: Format not recognised' >&2; exit 1", "Format not recognised"},
		{"too few clips", `echo '{"durationSec":1,"sampleRate":8000,"clips":[]}'`, "cut 0 clips, not the 1"},
		{"not JSON", "echo nope", "reading"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := fakePython(t, c.body)
			_, err := a.Cut(context.Background(), "x.wav", []Clip{{Path: "a.flac", EndSec: 3}})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

// TestCutOsprey runs analyzer/clip.py over test/2026-09-09 Osprey.wav (13.96 s
// of 32-bit float at 44.1 kHz), with the same Python as TestAnalyzeOsprey.
func TestCutOsprey(t *testing.T) {
	python := os.Getenv("BIRDSENSE_BIRDNET_PYTHON")
	if python == "" {
		t.Skip("BIRDSENSE_BIRDNET_PYTHON is not set")
	}
	source := filepath.Join("..", "..", "..", "test", "2026-09-09 Osprey.wav")
	a := Analyzer{Python: python, Script: filepath.Join("..", "..", "..", "analyzer", "analyze.py")}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	rec, err := a.Cut(ctx, source, []Clip{
		{Path: filepath.Join(dir, "a.flac"), StartSec: 2, EndSec: 5},
		{Path: filepath.Join(dir, "b.flac"), StartSec: 12, EndSec: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SampleRate != 44100 || rec.DurationSec != 13.96 {
		t.Errorf("recording is %v s at %d Hz, want 13.96 s at 44100 Hz", rec.DurationSec, rec.SampleRate)
	}
	if c := rec.Clips[1]; c.StartSec != 12 || c.EndSec != 13.96 {
		t.Errorf("second clip = %+v; want it clamped to the end of the recording", c)
	}

	// A mono 16-bit FLAC at the source's rate, holding 3 s: 132300 frames. All
	// four numbers are in STREAMINFO, the block after the "fLaC" magic and its
	// 4-byte block header, where the rate is the 20 bits at byte 18 and the
	// channel count, sample size and frame count run on from it unaligned.
	clip, err := os.ReadFile(rec.Clips[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(clip) < 26 || string(clip[:4]) != "fLaC" {
		t.Fatalf("clip is not a FLAC: %q", clip[:min(len(clip), 16)])
	}
	rate := (int(clip[18]) << 12) | (int(clip[19]) << 4) | (int(clip[20]) >> 4)
	channels := ((int(clip[20]) >> 1) & 0x07) + 1
	bits := ((int(clip[20]&0x01) << 4) | (int(clip[21]) >> 4)) + 1
	if channels != 1 || rate != 44100 || bits != 16 {
		t.Errorf("clip is %d channels at %d Hz, %d-bit; want mono, 44100 Hz, 16-bit", channels, rate, bits)
	}
	frames := int64(clip[21]&0x0F)<<32 | int64(clip[22])<<24 | int64(clip[23])<<16 | int64(clip[24])<<8 | int64(clip[25])
	if frames != 132300 {
		t.Errorf("clip holds %d frames, want %d: 3 s at 44100 Hz", frames, 132300)
	}
	// Lossless, but the point of FLAC is that it is smaller than saying it flat.
	if len(clip) >= 132300*2 {
		t.Errorf("clip is %d bytes, no smaller than the %d bytes of PCM in it", len(clip), 132300*2)
	}
}
