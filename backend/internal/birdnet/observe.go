package birdnet

import (
	"context"
	"os"
	"os/exec"
)

// Observer is told about each script Analyze and Cut run, so something
// outside this package can measure what a run cost: internal/perf samples the
// process group while it runs, and reads its resource usage when it exits.
// Each script runs in a process group of its own (on unix), whose id is the
// script's pid, so the pid names the script and every worker it starts.
type Observer interface {
	// Started is called once the script is running.
	Started(pid int)
	// Exited is called when it has exited, however it went.
	Exited(pid int, state *os.ProcessState)
}

type observerKey struct{}

// WithObserver returns a context under which Analyze and Cut report the
// scripts they run to o. It is a context value rather than a field of
// Analyzer or Options because it belongs to one call: several calls may be
// running on the same Analyzer, each measured separately.
func WithObserver(ctx context.Context, o Observer) context.Context {
	return context.WithValue(ctx, observerKey{}, o)
}

// run is cmd.Run, telling ctx's Observer, if there is one, what ran.
func run(ctx context.Context, cmd *exec.Cmd) error {
	o, _ := ctx.Value(observerKey{}).(Observer)
	if o == nil {
		return cmd.Run()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	o.Started(pid)
	err := cmd.Wait()
	o.Exited(pid, cmd.ProcessState)
	return err
}
