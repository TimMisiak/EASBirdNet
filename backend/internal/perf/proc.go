package perf

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// clockTicks is USER_HZ, the unit of utime and stime in /proc/{pid}/stat. It
// is 100 on every Linux this runs on; reading it properly needs cgo.
const clockTicks = 100

// proc is one process, as far as a sample needs it.
type proc struct {
	pid, pgid int
	// ticks is user plus system CPU time, in clockTicks.
	ticks uint64
}

// processes lists every process under procDir with its process group. A
// process that exits while it is being read is skipped. Where there is no
// /proc (not Linux) the list is empty.
func processes(procDir string) []proc {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procDir, e.Name(), "stat"))
		if err != nil {
			continue
		}
		if p, ok := parseStat(pid, string(b)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseStat reads the process group and CPU time out of a /proc/{pid}/stat
// line. The command name is in parentheses and may itself contain spaces and
// parentheses, so the fields are counted from the last ")".
func parseStat(pid int, line string) (proc, bool) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return proc{}, false
	}
	// After the name: state(3) ppid(4) pgrp(5) ... utime(14) stime(15).
	f := strings.Fields(line[end+1:])
	if len(f) < 13 {
		return proc{}, false
	}
	pgid, err1 := strconv.Atoi(f[2])
	utime, err2 := strconv.ParseUint(f[11], 10, 64)
	stime, err3 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return proc{}, false
	}
	return proc{pid: pid, pgid: pgid, ticks: utime + stime}, true
}

// pss is a process's proportional set size in bytes, from smaps_rollup, or
// its resident set from status where smaps_rollup can't be read. 0 if the
// process has gone.
func pss(procDir string, pid int) int64 {
	dir := filepath.Join(procDir, strconv.Itoa(pid))
	if kb, ok := kbField(filepath.Join(dir, "smaps_rollup"), "Pss:"); ok {
		return kb << 10
	}
	kb, _ := kbField(filepath.Join(dir, "status"), "VmRSS:")
	return kb << 10
}

// kbField finds "Name:   1234 kB" in a /proc file.
func kbField(path, name string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, name); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				n, err := strconv.ParseInt(f[0], 10, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}
