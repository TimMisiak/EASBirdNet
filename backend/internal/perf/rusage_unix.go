//go:build unix

package perf

import (
	"os"
	"runtime"
	"syscall"
)

// usage is the CPU seconds and peak resident set a finished process used,
// including every child it waited for -- birdnet's workers, which the script
// joins before it exits.
func usage(state *os.ProcessState) (cpuSec float64, maxRSS int64) {
	if state == nil {
		return 0, 0
	}
	cpuSec = state.UserTime().Seconds() + state.SystemTime().Seconds()
	if ru, ok := state.SysUsage().(*syscall.Rusage); ok {
		maxRSS = int64(ru.Maxrss)
		if runtime.GOOS != "darwin" {
			// Linux reports kilobytes; macOS reports bytes.
			maxRSS <<= 10
		}
	}
	return cpuSec, maxRSS
}
