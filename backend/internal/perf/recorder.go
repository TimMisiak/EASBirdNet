package perf

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults for Config's zero fields.
const (
	// DefaultInterval is how often a sample is taken while anything is
	// running. Reading /proc for every process costs a few milliseconds, and
	// a task lasts minutes, so five seconds sees its shape for next to nothing.
	DefaultInterval = 5 * time.Second
	// DefaultFlushEvery is how often the sink is flushed: in Azure, how much a
	// replica killed without warning loses.
	DefaultFlushEvery = 30 * time.Second
)

// Config is what a Recorder says about itself in its Run record.
type Config struct {
	// Instance names the process; Instance() is the usual choice.
	Instance string
	// Version is the image tag, if known.
	Version string
	// Settings are the analysis settings, recorded as they are.
	Settings map[string]any
	// LimitCores and LimitMemory are the replica's size as the deployment
	// configured it, used when the container's own limits can't be read --
	// in a sandbox VM, the CPUs the process sees are the VM's, not its quota.
	LimitCores  float64
	LimitMemory int64
	// Interval and FlushEvery default to DefaultInterval and
	// DefaultFlushEvery.
	Interval, FlushEvery time.Duration
}

// Instance names this process for the records: the Container Apps replica,
// which Azure puts in the environment, or else the hostname.
func Instance() string {
	if name := os.Getenv("CONTAINER_APP_REPLICA_NAME"); name != "" {
		return name
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "unknown"
}

// Recorder samples the container and the tasks it is told about, and writes
// records to a Sink. A nil *Recorder records nothing, so code that measures
// needn't check whether it is being measured.
type Recorder struct {
	sink       Sink
	log        *slog.Logger
	interval   time.Duration
	flushEvery time.Duration
	// cgroupDir and procDir are the real ones except in tests.
	cgroupDir, procDir string
	now                func() time.Time

	mu    sync.Mutex
	tasks map[*Tracker]struct{}
	seq   int
	// prev is the last container reading, taken every interval whether or
	// not a sample is written, so the first sample after an idle spell
	// measures the last interval and not the whole spell.
	prev   container
	prevAt time.Time
	// quiet is set once a sample has been written with nothing running.
	// Nothing more is written until a task starts: an idle web app would
	// otherwise write a line every few seconds, for ever, about nothing.
	quiet bool
	limit struct {
		cores  float64
		memory int64
	}
}

// New returns a Recorder that writes to sink, and writes its Run record.
func New(sink Sink, cfg Config, log *slog.Logger) *Recorder {
	return newRecorder(sink, cfg, log, cgroupDir, "/proc", time.Now)
}

func newRecorder(sink Sink, cfg Config, log *slog.Logger, cgroup, proc string, now func() time.Time) *Recorder {
	r := &Recorder{
		sink: sink, log: log,
		interval: cfg.Interval, flushEvery: cfg.FlushEvery,
		cgroupDir: cgroup, procDir: proc, now: now,
		tasks: map[*Tracker]struct{}{},
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	if r.flushEvery <= 0 {
		r.flushEvery = DefaultFlushEvery
	}
	c := r.read()
	r.prev, r.prevAt = c, now()
	from := "cgroup"
	r.limit.cores, r.limit.memory = c.limitCores, c.limitMemory
	if r.limit.cores == 0 || r.limit.memory == 0 {
		from = "config"
		if r.limit.cores == 0 {
			r.limit.cores = cfg.LimitCores
		}
		if r.limit.memory == 0 {
			r.limit.memory = cfg.LimitMemory
		}
	}
	if r.limit.cores == 0 || r.limit.memory == 0 {
		from = "host"
		if r.limit.cores == 0 {
			r.limit.cores = float64(runtime.NumCPU())
		}
		if r.limit.memory == 0 {
			r.limit.memory = hostMemory(r.procDir)
		}
	}
	run := Run{
		Type: TypeRun, T: now().UTC(), Instance: cfg.Instance, Version: cfg.Version,
		CPUModel: cpuModel(r.procDir), HostCPUs: runtime.NumCPU(), Source: c.source,
		LimitCores: r.limit.cores, LimitMemory: r.limit.memory, LimitsFrom: from, Settings: cfg.Settings,
	}
	if err := sink.Header(run); err != nil {
		log.Warn("perf: writing the run record", "err", err)
	}
	return r
}

// Run samples every interval and flushes the sink every flush interval until
// ctx ends, then flushes once more. Start it in its own goroutine.
func (r *Recorder) Run(ctx context.Context) {
	if r == nil {
		return
	}
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	lastFlush := r.now()
	for {
		select {
		case <-ctx.Done():
			r.flush(context.Background())
			return
		case <-tick.C:
		}
		r.sample()
		if r.now().Sub(lastFlush) >= r.flushEvery {
			r.flush(ctx)
			lastFlush = r.now()
		}
	}
}

// Flush writes whatever the sink holds.
func (r *Recorder) Flush(ctx context.Context) {
	if r != nil {
		r.flush(ctx)
	}
}

func (r *Recorder) flush(ctx context.Context) {
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.sink.Flush(fctx); err != nil {
		r.log.Warn("perf: flushing records", "err", err)
	}
}

// sample writes one Sample record, unless nothing has run since the last
// one said so.
func (r *Recorder) sample() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	c := r.read()
	prev, prevAt := r.prev, r.prevAt
	r.prev, r.prevAt = c, now
	if len(r.tasks) == 0 {
		if r.quiet {
			return
		}
		r.quiet = true
	} else {
		r.quiet = false
	}

	s := Sample{Type: TypeSample, T: now.UTC(), Tasks: []TaskSample{}}
	if c.ok {
		dt := now.Sub(prevAt).Seconds()
		cpu := &CPU{LimitCores: r.limit.cores, ThrottledMs: max(c.throttledUsec-prev.throttledUsec, 0) / 1000}
		if dt > 0 {
			cpu.UsedCores = round2(float64(max(c.usageUsec-prev.usageUsec, 0)) / 1e6 / dt)
		}
		s.CPU = cpu
		s.Mem = &Mem{Current: c.memCurrent, Anon: c.memAnon, File: c.memFile, Limit: r.limit.memory, OOMKills: c.oomKills}
	}
	if len(r.tasks) > 0 {
		byGroup := map[int][]proc{}
		for _, p := range processes(r.procDir) {
			byGroup[p.pgid] = append(byGroup[p.pgid], p)
		}
		for t := range r.tasks {
			s.Tasks = append(s.Tasks, t.sample(byGroup, now))
		}
	}
	if err := r.sink.Write(s); err != nil {
		r.log.Warn("perf: writing a sample", "err", err)
	}
}

func (r *Recorder) read() container {
	return readContainer(r.cgroupDir, r.procDir)
}

// Begin starts measuring one model's pass over a file. Pass the Tracker to
// birdnet.WithObserver so it sees the scripts the pass runs, mark each phase
// with Phase, and finish with End.
func (r *Recorder) Begin(model string) *Tracker {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	t := &Tracker{
		r: r, model: model, began: r.now(),
		id:     strconv.FormatInt(r.now().UnixMilli(), 36) + "-" + strconv.Itoa(r.seq),
		groups: map[int]bool{}, phases: map[string]float64{}, ticks: map[int]uint64{},
	}
	t.lastAt = t.began
	for other := range r.tasks {
		if t.running == nil {
			t.running = map[string]int{}
		}
		t.running[other.model]++
	}
	r.tasks[t] = struct{}{}
	return t
}

// Tracker measures one task. Its methods are safe on a nil Tracker, which
// measures nothing.
type Tracker struct {
	r       *Recorder
	id      string
	model   string
	began   time.Time
	running map[string]int

	mu sync.Mutex
	// groups are the process groups the task has started, true while running.
	groups  map[int]bool
	phase   string
	phaseAt time.Time
	phases  map[string]float64
	cpuSec  float64
	maxRSS  int64
	peakPSS int64
	// ticks is each process's CPU at the last sample, and lastAt when that
	// was: a new process started from nothing, so its whole count is new.
	ticks  map[int]uint64
	lastAt time.Time
}

// Started is birdnet.Observer's: a script started, in its own process group.
func (t *Tracker) Started(pid int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.groups[pid] = true
}

// Exited is birdnet.Observer's: the script has exited, and the kernel's
// account of it is exact where the samples are not.
func (t *Tracker) Exited(pid int, state *os.ProcessState) {
	if t == nil {
		return
	}
	cpu, rss := usage(state)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.groups[pid] = false
	t.cpuSec += cpu
	t.maxRSS = max(t.maxRSS, rss)
}

// Phase ends the current phase, if any, and starts the named one.
func (t *Tracker) Phase(name string) {
	if t == nil {
		return
	}
	now := t.r.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endPhase(now)
	t.phase, t.phaseAt = name, now
}

func (t *Tracker) endPhase(now time.Time) {
	if t.phase != "" {
		t.phases[t.phase] += now.Sub(t.phaseAt).Seconds()
		t.phase = ""
	}
}

// sample sums the task's running process groups. The recorder holds its own
// lock; this takes the task's.
func (t *Tracker) sample(byGroup map[int][]proc, now time.Time) TaskSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TaskSample{ID: t.id, Model: t.model, Phase: t.phase}
	var used uint64
	seen := make(map[int]uint64, len(t.ticks))
	for pgid, live := range t.groups {
		if !live {
			continue
		}
		for _, p := range byGroup[pgid] {
			used += p.ticks - min(t.ticks[p.pid], p.ticks)
			seen[p.pid] = p.ticks
			s.PSS += pss(t.r.procDir, p.pid)
			s.Procs++
		}
	}
	// A process that exited since the last sample took what it used in that
	// interval with it, so the figure is a slight underestimate then;
	// Task.CPUSec is the exact one.
	if dt := now.Sub(t.lastAt).Seconds(); dt > 0 {
		s.Cores = round2(float64(used) / clockTicks / dt)
	}
	t.ticks, t.lastAt = seen, now
	t.peakPSS = max(t.peakPSS, s.PSS)
	return s
}

// End finishes the task, writes its Task record and returns it. The caller
// fills in what only it knows -- the card and file, the result, the audio --
// and End adds the timings, the CPU and the memory.
func (t *Tracker) End(rec Task) Task {
	if t == nil {
		return rec
	}
	r := t.r
	now := r.now()
	r.mu.Lock()
	delete(r.tasks, t)
	r.mu.Unlock()

	t.mu.Lock()
	t.endPhase(now)
	rec.Type, rec.T, rec.ID, rec.Model = TypeTask, now.UTC(), t.id, t.model
	rec.WallSec = round2(now.Sub(t.began).Seconds())
	rec.Phases = map[string]float64{}
	for k, v := range t.phases {
		rec.Phases[k] = round2(v)
	}
	rec.CPUSec, rec.MaxRSS, rec.PeakPSS = round2(t.cpuSec), t.maxRSS, t.peakPSS
	rec.RunningAtStart = t.running
	t.mu.Unlock()
	rec.Error = firstLine(rec.Error)

	if err := r.sink.Write(rec); err != nil {
		r.log.Warn("perf: writing a task", "err", err)
	}
	attrs := []any{"model", rec.Model, "card", rec.Card, "path", rec.Path, "result", rec.Result,
		"audio_sec", rec.AudioSec, "wall_sec", rec.WallSec, "cpu_sec", rec.CPUSec,
		"peak_pss_mb", rec.PeakPSS >> 20, "max_rss_mb", rec.MaxRSS >> 20, "phases", rec.Phases}
	if rec.CPUSec > 0 && rec.AudioSec > 0 {
		attrs = append(attrs, "audio_per_cpu_sec", round2(rec.AudioSec/rec.CPUSec))
	}
	r.log.Info("analysis: task", attrs...)
	return rec
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	const most = 300
	if len(s) > most {
		s = s[:most] + "…"
	}
	return s
}
