//go:build unix

package birdnet

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel starts the script in its own process group and, on
// cancellation, kills the whole group. birdnet runs inference in child
// processes, and killing only the script would orphan them mid-file.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
