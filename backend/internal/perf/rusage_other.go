//go:build !unix

package perf

import "os"

func usage(state *os.ProcessState) (cpuSec float64, maxRSS int64) {
	if state == nil {
		return 0, 0
	}
	return state.UserTime().Seconds() + state.SystemTime().Seconds(), 0
}
