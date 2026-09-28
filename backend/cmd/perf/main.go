// Command perf summarizes the analysis performance records internal/perf
// writes -- from the server, under perf/ in file storage, or from
// `cmd/analyze -bench` -- into text short enough to paste into a
// conversation about how big the analysis replica should be (ANALYSIS.md,
// *Performance data*).
//
//	az storage blob download-batch --auth-mode login --account-name ACCOUNT \
//	  -s audio -d ./perf-data --pattern 'perf/2026-10-*'
//	go run ./cmd/perf ./perf-data
//
// Arguments are files or directories, searched for *.jsonl.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/perf"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: perf FILE_OR_DIR...")
		os.Exit(2)
	}
	d, err := load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "perf:", err)
		os.Exit(1)
	}
	report(os.Stdout, d)
}

// sample is a Sample with the instance whose run it came under.
type sample struct {
	instance string
	perf.Sample
}

type data struct {
	runs    map[string]perf.Run // by instance and start, so repeated headers count once
	tasks   []perf.Task
	samples []sample
	files   int
	skipped int
}

// load reads every record under paths. A line that isn't a record is
// counted, not fatal: a segment cut off mid-write ends in half a line.
func load(paths []string) (*data, error) {
	d := &data{runs: map[string]perf.Run{}}
	seenTask := map[string]bool{}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			d.files++
			return d.read(path, seenTask)
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(d.samples, func(a, b sample) int {
		if c := strings.Compare(a.instance, b.instance); c != 0 {
			return c
		}
		return a.T.Compare(b.T)
	})
	return d, nil
}

func (d *data) read(path string, seenTask map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	instance := ""
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var head struct {
			Type string `json:"type"`
		}
		line := s.Bytes()
		if json.Unmarshal(line, &head) != nil {
			d.skipped++
			continue
		}
		switch head.Type {
		case perf.TypeRun:
			var r perf.Run
			if json.Unmarshal(line, &r) == nil {
				instance = r.Instance
				d.runs[r.Instance+"@"+r.T.String()] = r
			}
		case perf.TypeTask:
			var t perf.Task
			if json.Unmarshal(line, &t) == nil && !seenTask[instance+t.ID] {
				seenTask[instance+t.ID] = true
				d.tasks = append(d.tasks, t)
			}
		case perf.TypeSample:
			var smp perf.Sample
			if json.Unmarshal(line, &smp) == nil {
				d.samples = append(d.samples, sample{instance, smp})
			}
		default:
			d.skipped++
		}
	}
	return s.Err()
}

func report(w io.Writer, d *data) {
	fmt.Fprintf(w, "%d file(s), %d run header(s), %d task(s), %d sample(s)", d.files, len(d.runs), len(d.tasks), len(d.samples))
	if d.skipped > 0 {
		fmt.Fprintf(w, ", %d unreadable line(s)", d.skipped)
	}
	fmt.Fprintln(w)
	reportMachines(w, d)
	reportModels(w, d)
	reportInstances(w, d)
}

// reportMachines says what the figures were measured on: a core on one CPU
// isn't a core on another.
func reportMachines(w io.Writer, d *data) {
	type machine struct {
		cpu             string
		cores           float64
		memory          int64
		version         string
		settings        string
		instances, runs int
	}
	byKey := map[string]*machine{}
	instances := map[string]map[string]bool{}
	for _, r := range d.runs {
		settings, _ := json.Marshal(r.Settings)
		key := fmt.Sprintf("%s|%v|%d|%s|%s", r.CPUModel, r.LimitCores, r.LimitMemory, r.Version, settings)
		m := byKey[key]
		if m == nil {
			m = &machine{cpu: r.CPUModel, cores: r.LimitCores, memory: r.LimitMemory, version: r.Version, settings: string(settings)}
			byKey[key] = m
			instances[key] = map[string]bool{}
		}
		m.runs++
		instances[key][r.Instance] = true
	}
	fmt.Fprintln(w, "\nMeasured on")
	tw := table(w, "CPU\tLIMIT\tIMAGE\tINSTANCES\tSETTINGS")
	keys := sortedKeys(byKey)
	for _, k := range keys {
		m := byKey[k]
		fmt.Fprintf(tw, "%s\t%.2g vCPU / %s\t%s\t%d\t%s\n", orDash(m.cpu), m.cores, gb(m.memory), orDash(m.version), len(instances[k]), m.settings)
	}
	tw.Flush()
}

// reportModels is the table the sizing comes from: per model and setting,
// how much audio a CPU-second gets through and how much memory a task needs.
func reportModels(w io.Writer, d *data) {
	type key struct {
		model            string
		workers, threads int
	}
	groups := map[key][]perf.Task{}
	failed := map[key]int{}
	for _, t := range d.tasks {
		k := key{t.Model, t.Workers, t.Threads}
		if t.Result != "ok" {
			failed[k]++
			continue
		}
		groups[k] = append(groups[k], t)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	for k := range failed {
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := strings.Compare(a.model, b.model); c != 0 {
			return c
		}
		if a.workers != b.workers {
			return a.workers - b.workers
		}
		return a.threads - b.threads
	})

	fmt.Fprintln(w, "\nPer model and setting (tasks that succeeded)")
	fmt.Fprintln(w, "  audio/CPU-s: seconds of audio per CPU-second, median (slowest tenth). x real time: audio over wall time.")
	fmt.Fprintln(w, "  cores: CPU-seconds over wall seconds. PSS: peak memory of the task's processes together.")
	tw := table(w, "MODEL\tWORKERS\tTHREADS\tTASKS\tFAILED\tAUDIO\tAUDIO/CPU-S\tx REAL TIME\tCORES\tPSS p50\tPSS p95\tPSS max\tMAX RSS\tWALL SPLIT")
	for _, k := range keys {
		ts := groups[k]
		var audio, perCPU, realtime, cores []float64
		var pss []float64
		var maxRSS int64
		phases := map[string]float64{}
		var wall float64
		for _, t := range ts {
			audio = append(audio, t.AudioSec)
			if t.CPUSec > 0 && t.AudioSec > 0 {
				perCPU = append(perCPU, t.AudioSec/t.CPUSec)
			}
			if t.WallSec > 0 {
				if t.AudioSec > 0 {
					realtime = append(realtime, t.AudioSec/t.WallSec)
				}
				cores = append(cores, t.CPUSec/t.WallSec)
			}
			if t.PeakPSS > 0 {
				pss = append(pss, float64(t.PeakPSS))
			}
			maxRSS = max(maxRSS, t.MaxRSS)
			for p, sec := range t.Phases {
				phases[p] += sec
			}
			wall += t.WallSec
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.model, k.workers, k.threads, len(ts), failed[k], hours(sum(audio)),
			withTail(perCPU), num(pct(realtime, 50)), num(pct(cores, 50)),
			gbf(pct(pss, 50)), gbf(pct(pss, 95)), gbf(pct(pss, 100)), gb(maxRSS), split(phases, wall))
	}
	tw.Flush()
}

// reportInstances is how full each replica was while it had work: the
// container's CPU against its limit, and memory against its limit.
func reportInstances(w io.Writer, d *data) {
	type stats struct {
		busySec                    float64
		util                       [4]float64 // seconds at <25%, <50%, <75%, >=75% of the CPU limit
		coreSec                    float64
		peakCurrent, peakAnon, lim int64
		oomFirst, oomLast          int64
		throttledMs                int64
		first, last                time.Time
		maxTasks                   int
	}
	byInstance := map[string]*stats{}
	var prev sample
	for i, s := range d.samples {
		st := byInstance[s.instance]
		if st == nil {
			st = &stats{first: s.T, oomFirst: -1}
			byInstance[s.instance] = st
		}
		st.last = s.T
		if s.Mem != nil {
			st.peakCurrent = max(st.peakCurrent, s.Mem.Current)
			st.peakAnon = max(st.peakAnon, s.Mem.Anon)
			st.lim = s.Mem.Limit
			if st.oomFirst < 0 {
				st.oomFirst = s.Mem.OOMKills
			}
			st.oomLast = s.Mem.OOMKills
		}
		st.maxTasks = max(st.maxTasks, len(s.Tasks))
		// A sample covers the interval before it; the first of an instance,
		// or one after a gap (the recorder stops sampling when idle), has no
		// interval it can speak for.
		if i == 0 || prev.instance != s.instance || s.CPU == nil || len(s.Tasks) == 0 {
			prev = s
			continue
		}
		dt := s.T.Sub(prev.T).Seconds()
		prev = s
		if dt <= 0 || dt > 60 {
			continue
		}
		st.busySec += dt
		st.coreSec += s.CPU.UsedCores * dt
		st.throttledMs += s.CPU.ThrottledMs
		frac := 0.0
		if s.CPU.LimitCores > 0 {
			frac = s.CPU.UsedCores / s.CPU.LimitCores
		}
		st.util[min(int(frac*4), 3)] += dt
	}

	fmt.Fprintln(w, "\nPer instance, while it had work")
	fmt.Fprintln(w, "  CPU: mean cores used, and the share of busy time spent at <25 / <50 / <75 / >=75% of the limit.")
	tw := table(w, "INSTANCE\tFROM\tBUSY\tMAX TASKS\tMEAN CORES\tCPU <25/<50/<75/>=75%\tTHROTTLED\tPEAK MEM\tPEAK ANON\tLIMIT\tOOM KILLS")
	for _, name := range sortedKeys(byInstance) {
		st := byInstance[name]
		mean := 0.0
		if st.busySec > 0 {
			mean = st.coreSec / st.busySec
		}
		var shares []string
		for _, sec := range st.util {
			shares = append(shares, fmt.Sprintf("%.0f", 100*sec/max(st.busySec, 1e-9)))
		}
		oom := int64(0)
		if st.oomFirst >= 0 {
			oom = st.oomLast - st.oomFirst
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%.2f\t%s\t%.0f s\t%s\t%s\t%s\t%d\n",
			name, st.first.Format("2006-01-02 15:04"), hours(st.busySec), st.maxTasks, mean,
			strings.Join(shares, "/"), float64(st.throttledMs)/1000, gb(st.peakCurrent), gb(st.peakAnon), gb(st.lim), oom)
	}
	tw.Flush()
}

func table(w io.Writer, header string) *tabwriter.Writer {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	return tw
}

// pct is the p-th percentile of v by nearest rank, or NaN-free 0 for none.
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	i := int(p/100*float64(len(s))+0.5) - 1
	return s[min(max(i, 0), len(s)-1)]
}

func withTail(v []float64) string {
	if len(v) == 0 {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", num(pct(v, 50)), num(pct(v, 10)))
}

func sum(v []float64) float64 {
	var t float64
	for _, x := range v {
		t += x
	}
	return t
}

// split is each phase's share of the wall time, largest first.
func split(phases map[string]float64, wall float64) string {
	if wall <= 0 {
		return "-"
	}
	names := sortedKeys(phases)
	slices.SortStableFunc(names, func(a, b string) int {
		switch {
		case phases[a] > phases[b]:
			return -1
		case phases[a] < phases[b]:
			return 1
		}
		return 0
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s %.0f%%", n, 100*phases[n]/wall)
	}
	return strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func num(v float64) string {
	switch {
	case v == 0:
		return "-"
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func hours(sec float64) string {
	if sec < 3600 {
		return fmt.Sprintf("%.1f min", sec/60)
	}
	return fmt.Sprintf("%.1f h", sec/3600)
}

func gb(b int64) string { return gbf(float64(b)) }

func gbf(b float64) string {
	if b <= 0 {
		return "-"
	}
	if b < 1<<30 {
		return fmt.Sprintf("%.0f MB", b/(1<<20))
	}
	return fmt.Sprintf("%.2f GB", b/(1<<30))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
