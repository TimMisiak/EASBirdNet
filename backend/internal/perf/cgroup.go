package perf

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupDir is where a process sees its own cgroup in a container.
const cgroupDir = "/sys/fs/cgroup"

// Where a reading of the container came from (Run.Source).
const (
	// SourceCgroup2 is the container's own cgroup v2 files: its CPU time,
	// throttling, memory and limits exactly.
	SourceCgroup2 = "cgroup2"
	// SourceCgroup1 is the same from cgroup v1's cpu, cpuacct and memory
	// controllers.
	SourceCgroup1 = "cgroup1"
	// SourceProc is the whole machine, from /proc/stat and /proc/meminfo,
	// when no cgroup can be read. Where each replica is a small VM of its own
	// -- which Container Apps' figures suggest -- the machine is the replica,
	// near enough; but its limits aren't visible there (Config.LimitCores).
	SourceProc = "proc"
)

// container is one reading of the container's CPU and memory. ok is false
// when nothing could be read at all.
type container struct {
	ok     bool
	source string
	// limitCores is the CPU quota over its period; 0 for no limit.
	limitCores float64
	// usageUsec and throttledUsec are cumulative.
	usageUsec, throttledUsec int64
	// limitMemory is 0 for no limit.
	limitMemory                  int64
	memCurrent, memAnon, memFile int64
	oomKills                     int64
}

// readContainer reads cgroup v2 under cgroup, else cgroup v1 there, else the
// machine from proc.
func readContainer(cgroup, proc string) container {
	if c, ok := readCgroup2(cgroup); ok {
		return c
	}
	if c, ok := readCgroup1(cgroup); ok {
		return c
	}
	return readProc(proc)
}

func readCgroup2(dir string) (container, bool) {
	c := container{ok: true, source: SourceCgroup2}
	usage, err := keyed(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return container{}, false
	}
	c.usageUsec, c.throttledUsec = usage["usage_usec"], usage["throttled_usec"]
	if f := fields(filepath.Join(dir, "cpu.max")); len(f) == 2 && f[0] != "max" {
		quota, err1 := strconv.ParseFloat(f[0], 64)
		period, err2 := strconv.ParseFloat(f[1], 64)
		if err1 == nil && err2 == nil && period > 0 {
			c.limitCores = quota / period
		}
	}
	if f := fields(filepath.Join(dir, "memory.max")); len(f) == 1 && f[0] != "max" {
		c.limitMemory, _ = strconv.ParseInt(f[0], 10, 64)
	}
	c.memCurrent = number(filepath.Join(dir, "memory.current"))
	if stat, err := keyed(filepath.Join(dir, "memory.stat")); err == nil {
		c.memAnon, c.memFile = stat["anon"], stat["file"]
	}
	if events, err := keyed(filepath.Join(dir, "memory.events")); err == nil {
		c.oomKills = events["oom_kill"]
	}
	return c, true
}

// readCgroup1 reads the v1 controllers, which are mounted one directory each
// (cpu and cpuacct often together, under either name).
func readCgroup1(root string) (container, bool) {
	var cpu, mem string
	for _, name := range []string{"cpu,cpuacct", "cpuacct,cpu", "cpuacct"} {
		if _, err := os.Stat(filepath.Join(root, name, "cpuacct.usage")); err == nil {
			cpu = filepath.Join(root, name)
			break
		}
	}
	if cpu == "" {
		return container{}, false
	}
	c := container{ok: true, source: SourceCgroup1}
	c.usageUsec = number(filepath.Join(cpu, "cpuacct.usage")) / 1000
	// The quota is in the cpu controller, which is usually the same
	// directory; look beside cpuacct first, then under its own name.
	for _, dir := range []string{cpu, filepath.Join(root, "cpu")} {
		quota, period := number(filepath.Join(dir, "cpu.cfs_quota_us")), number(filepath.Join(dir, "cpu.cfs_period_us"))
		if period > 0 {
			if quota > 0 {
				c.limitCores = float64(quota) / float64(period)
			}
			if stat, err := keyed(filepath.Join(dir, "cpu.stat")); err == nil {
				c.throttledUsec = stat["throttled_time"] / 1000
			}
			break
		}
	}
	if _, err := os.Stat(filepath.Join(root, "memory", "memory.usage_in_bytes")); err == nil {
		mem = filepath.Join(root, "memory")
		c.memCurrent = number(filepath.Join(mem, "memory.usage_in_bytes"))
		// No limit is reported as a huge number, not "max".
		if limit := number(filepath.Join(mem, "memory.limit_in_bytes")); limit > 0 && limit < 1<<60 {
			c.limitMemory = limit
		}
		if stat, err := keyed(filepath.Join(mem, "memory.stat")); err == nil {
			c.memAnon, c.memFile = stat["total_rss"], stat["total_cache"]
		}
		if oom, err := keyed(filepath.Join(mem, "memory.oom_control")); err == nil {
			c.oomKills = oom["oom_kill"]
		}
	}
	return c, true
}

// readProc reads the whole machine: CPU time from /proc/stat, memory from
// /proc/meminfo. There are no limits, throttling or OOM counts to read here.
func readProc(dir string) container {
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return container{}
	}
	c := container{ok: true, source: SourceProc}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 8 || f[0] != "cpu" {
		return container{}
	}
	// user nice system idle iowait irq softirq steal: busy is all but idle
	// and iowait.
	var ticks int64
	for i, v := range f[1:9] {
		if i == 3 || i == 4 {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		ticks += n
	}
	c.usageUsec = ticks * 1e6 / clockTicks
	info := meminfo(dir)
	c.memCurrent = info["MemTotal"] - info["MemAvailable"]
	c.memAnon, c.memFile = info["AnonPages"], info["Cached"]
	return c
}

// meminfo is /proc/meminfo in bytes.
func meminfo(dir string) map[string]int64 {
	out := map[string]int64{}
	b, err := os.ReadFile(filepath.Join(dir, "meminfo"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(rest); len(f) > 0 {
			kb, _ := strconv.ParseInt(f[0], 10, 64)
			out[k] = kb << 10
		}
	}
	return out
}

// hostMemory is MemTotal from /proc/meminfo, the limit when the container
// has none of its own.
func hostMemory(procDir string) int64 {
	return meminfo(procDir)["MemTotal"]
}

// number is a file holding one integer, or 0.
func number(path string) int64 {
	if f := fields(path); len(f) == 1 {
		n, _ := strconv.ParseInt(f[0], 10, 64)
		return n
	}
	return 0
}

// fields is a one-line file split on spaces, or nil if it can't be read.
func fields(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// keyed reads a file of "key value" lines, the shape of cpu.stat,
// memory.stat and memory.events.
func keyed(path string) (map[string]int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		k, v, ok := strings.Cut(s.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out, nil
}

// cpuModel is the first "model name" in /proc/cpuinfo: what a core was, so
// measurements from two machines aren't compared as if they were one.
func cpuModel(procDir string) string {
	b, err := os.ReadFile(filepath.Join(procDir, "cpuinfo"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
