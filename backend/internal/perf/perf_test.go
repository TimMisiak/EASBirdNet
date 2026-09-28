package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

func TestParseStat(t *testing.T) {
	// A command name can hold spaces and parentheses; fields count from the
	// last ")".
	line := "4242 (python (worker) 1) S 1 4200 4200 0 -1 4194560 100 0 0 0 250 30 0 0 20 0 3 0 100 0 0"
	p, ok := parseStat(4242, line)
	if !ok || p.pgid != 4200 || p.ticks != 280 {
		t.Errorf("parseStat = %+v, %v; want group 4200, 280 ticks", p, ok)
	}
	if _, ok := parseStat(1, "garbage"); ok {
		t.Error("garbage parsed")
	}
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadContainer(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"cpu.stat":       "usage_usec 5000000\nuser_usec 4000000\nthrottled_usec 250000\n",
		"cpu.max":        "400000 100000\n",
		"memory.max":     "8589934592\n",
		"memory.current": "4294967296\n",
		"memory.stat":    "anon 3221225472\nfile 1073741824\n",
		"memory.events":  "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n",
	})
	c := readContainer(dir)
	if !c.ok || c.limitCores != 4 || c.usageUsec != 5000000 || c.throttledUsec != 250000 ||
		c.limitMemory != 8<<30 || c.memCurrent != 4<<30 || c.memAnon != 3<<30 || c.memFile != 1<<30 || c.oomKills != 1 {
		t.Errorf("container = %+v", c)
	}

	write(t, dir, map[string]string{"cpu.max": "max 100000\n", "memory.max": "max\n"})
	if c := readContainer(dir); c.limitCores != 0 || c.limitMemory != 0 {
		t.Errorf("unlimited container = %+v; want no limits", c)
	}
	if c := readContainer(filepath.Join(dir, "absent")); c.ok {
		t.Error("a missing cgroup read as present")
	}
}

// A task is sampled through its process groups, and its record carries the
// phases, the exact CPU from Exited and the peak memory the samples saw.
func TestTrackerFollowsItsProcessGroup(t *testing.T) {
	cg, proc := t.TempDir(), t.TempDir()
	write(t, cg, map[string]string{"cpu.stat": "usage_usec 0\n", "cpu.max": "200000 100000\n", "memory.max": "4294967296\n"})
	stat := func(pid, pgid, utime int) string {
		return strings.Join([]string{strconv.Itoa(pid), "(python)", "S", "1", strconv.Itoa(pgid), strconv.Itoa(pgid), "0", "-1", "0", "0", "0", "0", "0", strconv.Itoa(utime), "0"}, " ")
	}
	write(t, proc, map[string]string{
		"100/stat":         stat(100, 100, 0),
		"100/smaps_rollup": "Rss: 900 kB\nPss:    102400 kB\n",
		"101/stat":         stat(101, 100, 0),
		"101/smaps_rollup": "Pss:    51200 kB\n",
		"200/stat":         stat(200, 200, 500), // someone else's
		"200/smaps_rollup": "Pss:    999999 kB\n",
	})
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	r := newRecorder(NewWriterSink(&out), Config{Instance: "replica-1", Settings: map[string]any{"perch": true}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), cg, proc, clock)

	other := r.Begin("birdnet")
	task := r.Begin("perch")
	task.Phase("download")
	now = now.Add(2 * time.Second)
	task.Phase("analyze")
	task.Started(100)
	now = now.Add(5 * time.Second)
	// Both processes used a second and a half of CPU in those five seconds.
	write(t, proc, map[string]string{"100/stat": stat(100, 100, 150), "101/stat": stat(101, 100, 150)})
	write(t, cg, map[string]string{"cpu.stat": "usage_usec 3000000\n"})
	r.sample()
	task.Exited(100, nil)
	now = now.Add(time.Second)
	rec := task.End(Task{Path: "a.wav", Result: "ok", AudioSec: 3600})
	other.End(Task{Result: "ok"})

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var run Run
	if err := json.Unmarshal([]byte(lines[0]), &run); err != nil || run.Type != TypeRun || run.Instance != "replica-1" ||
		run.LimitCores != 2 || run.LimitMemory != 4<<30 || !run.Cgroup {
		t.Errorf("run = %+v, %v", run, err)
	}
	var s Sample
	if err := json.Unmarshal([]byte(lines[1]), &s); err != nil {
		t.Fatal(err)
	}
	var mine TaskSample
	for _, ts := range s.Tasks {
		if ts.Model == "perch" {
			mine = ts
		}
	}
	if mine.Procs != 2 || mine.PSS != 150<<20 || mine.Cores != 0.43 || mine.Phase != "analyze" {
		t.Errorf("task sample = %+v; want 2 processes, 150 MB, 3 s of CPU over 7 s", mine)
	}
	if s.CPU == nil || s.CPU.UsedCores != 0.43 || s.CPU.LimitCores != 2 {
		t.Errorf("container cpu = %+v", s.CPU)
	}
	if rec.Model != "perch" || rec.WallSec != 8 || rec.Phases["download"] != 2 || rec.Phases["analyze"] != 6 ||
		rec.PeakPSS != 150<<20 || rec.RunningAtStart["birdnet"] != 1 || rec.AudioSec != 3600 {
		t.Errorf("task = %+v", rec)
	}
}

// Records are held in a segment each flush rewrites, which rolls once it is
// old enough; every segment starts with the run record.
func TestStoreSinkSegments(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 23, 58, 0, 0, time.UTC)
	s := NewStoreSink(store, "replica/1")
	s.now = func() time.Time { return now }
	ctx := context.Background()
	if err := s.Header(Run{Type: TypeRun, Instance: "replica/1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal("flushing nothing:", err)
	}
	s.Write(Task{Type: TypeTask, ID: "a"})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	first := storage.PerfName("2026-10-01", "replica/1", "20261001T235800Z")
	now = now.Add(segmentAge)
	s.Write(Task{Type: TypeTask, ID: "b"})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s.Write(Task{Type: TypeTask, ID: "c"})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	second := storage.PerfName("2026-10-02", "replica/1", "20261002T000900Z")

	read := func(name string) []string {
		r, err := store.Open(ctx, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	if got := read(first); len(got) != 3 || !strings.Contains(got[0], `"run"`) || !strings.Contains(got[2], `"b"`) {
		t.Errorf("first segment = %q; want the header and tasks a and b", got)
	}
	if got := read(second); len(got) != 2 || !strings.Contains(got[0], `"run"`) || !strings.Contains(got[1], `"c"`) {
		t.Errorf("second segment = %q; want the header and task c", got)
	}
	if strings.Contains(first, "replica/1") {
		t.Errorf("instance not kept to one path element: %s", first)
	}
}

// A nil Recorder and Tracker record nothing and don't crash, so the queue
// needn't check.
func TestNilRecorder(t *testing.T) {
	var r *Recorder
	task := r.Begin("birdnet")
	task.Phase("analyze")
	task.Started(1)
	task.Exited(1, nil)
	if rec := task.End(Task{Result: "ok"}); rec.Result != "ok" {
		t.Errorf("End = %+v", rec)
	}
	r.Flush(context.Background())
}
