package perf

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupDir is where a process sees its own cgroup v2 in a container.
const cgroupDir = "/sys/fs/cgroup"

// container is one reading of the container's cgroup v2 files. Missing files
// leave zeros, and ok false when there is no cgroup v2 there at all.
type container struct {
	ok bool
	// limitCores is cpu.max's quota over its period; 0 for no limit.
	limitCores float64
	// usageUsec and throttledUsec are cumulative, from cpu.stat.
	usageUsec, throttledUsec int64
	// limitMemory is memory.max; 0 for no limit.
	limitMemory                  int64
	memCurrent, memAnon, memFile int64
	oomKills                     int64
}

func readContainer(dir string) container {
	var c container
	usage, err := keyed(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return c
	}
	c.ok = true
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
	if f := fields(filepath.Join(dir, "memory.current")); len(f) == 1 {
		c.memCurrent, _ = strconv.ParseInt(f[0], 10, 64)
	}
	if stat, err := keyed(filepath.Join(dir, "memory.stat")); err == nil {
		c.memAnon, c.memFile = stat["anon"], stat["file"]
	}
	if events, err := keyed(filepath.Join(dir, "memory.events")); err == nil {
		c.oomKills = events["oom_kill"]
	}
	return c
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

// hostMemory is MemTotal from /proc/meminfo, the limit when the container
// has none of its own.
func hostMemory(procDir string) int64 {
	b, err := os.ReadFile(filepath.Join(procDir, "meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
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
