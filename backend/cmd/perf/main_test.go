package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A folder of segments, one cut off mid-line, is read and summarized; a task
// that appears in two segments of the same replica counts once.
func TestSummary(t *testing.T) {
	dir := t.TempDir()
	run := `{"type":"run","t":"2026-10-01T10:00:00Z","instance":"r1","cpuModel":"Test CPU","limitCores":4,"limitMemory":8589934592,"cgroup":true,"settings":{"perch":true}}`
	sample := func(t string, used float64, tasks string) string {
		return `{"type":"sample","t":"` + t + `","cpu":{"usedCores":` + strconv.FormatFloat(used, 'f', -1, 64) + `,"limitCores":4,"throttledMs":0},"mem":{"current":6442450944,"anon":5368709120,"file":1073741824,"limit":8589934592,"oomKills":0},"tasks":[` + tasks + `]}`
	}
	perch := `{"id":"p","model":"perch","cores":1,"pss":2684354560,"procs":2}`
	task := `{"type":"task","t":"2026-10-01T10:01:00Z","id":"p","model":"perch","workers":1,"threads":0,"result":"ok","audioSec":3600,"detections":3,"wallSec":450,"phases":{"download":10,"analyze":420,"clip":20},"cpuSec":440,"peakPss":2684354560,"maxRss":2147483648}`
	failed := `{"type":"task","t":"2026-10-01T10:02:00Z","id":"q","model":"birdnet","workers":1,"threads":0,"result":"error","error":"boom","wallSec":5,"phases":{},"cpuSec":1,"peakPss":0,"maxRss":0}`
	files := map[string]string{
		"perf/2026-10-01/r1/a.jsonl": strings.Join([]string{run,
			sample("2026-10-01T10:00:05Z", 1, perch),
			sample("2026-10-01T10:00:10Z", 1, perch),
			sample("2026-10-01T10:00:15Z", 3.5, perch),
			task}, "\n") + "\n",
		"perf/2026-10-01/r1/b.jsonl": run + "\n" + task + "\n" + failed + "\n" + `{"type":"sam`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}

	d, err := load([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if d.files != 2 || len(d.runs) != 1 || len(d.tasks) != 2 || len(d.samples) != 3 || d.skipped != 1 {
		t.Fatalf("loaded %d files, %d runs, %d tasks, %d samples, %d skipped", d.files, len(d.runs), len(d.tasks), len(d.samples), d.skipped)
	}
	var out bytes.Buffer
	report(&out, d)
	got := out.String()
	for _, want := range []string{
		"Test CPU", "4 vCPU / 8.00 GB",
		// 3600 s of audio over 440 CPU-seconds; 450 s of wall time.
		"perch    1        0        1      0       1.0 h    8.18 (8.18)  8.00         0.98   2.50 GB",
		"analyze 93%, clip 4%, download 2%",
		"birdnet  1        0        0      1",
		// 10 s busy: 5 s at 1 core of 4 (<50%), 5 s at 3.5 (>=75%).
		"r1        2026-10-01 10:00  0.2 min  1          2.25        0/50/0/50",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}
