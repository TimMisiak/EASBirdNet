// Package perf records what analysis costs the machine it runs on, so the
// replica that runs it can be sized from measurements (ANALYSIS.md,
// *Performance data*).
//
// A Recorder writes JSON Lines, one record per line, of three types:
//
//   - Run, once when the recorder starts, and again at the top of every
//     segment a StoreSink writes, so any one file says what it was measured on.
//   - Sample, every few seconds while anything is being analyzed: the whole
//     container's CPU and memory from cgroup v2, and each running task's from
//     its process group under /proc.
//   - Task, once per model's pass over a file: how long each phase took, the
//     CPU it used, and its peak memory.
//
// cmd/perf summarizes them. Nothing reads them back into the app: they are for
// a person deciding how big a replica should be.
package perf

import "time"

// The Type of each record.
const (
	TypeRun    = "run"
	TypeSample = "sample"
	TypeTask   = "task"
)

// Run describes the process and the machine the records after it were
// measured on.
type Run struct {
	Type string    `json:"type"`
	T    time.Time `json:"t"`
	// Instance is the process: the replica name in Azure, the hostname
	// otherwise. It is also the directory a StoreSink writes under.
	Instance string `json:"instance"`
	// Version is the image tag, when the environment says.
	Version  string `json:"version,omitempty"`
	CPUModel string `json:"cpuModel,omitempty"`
	// HostCPUs is how many CPUs the process can be scheduled on, which in a
	// container is usually the host's and not its limit.
	HostCPUs int `json:"hostCpus"`
	// Source is where the container's figures come from: SourceCgroup2,
	// SourceCgroup1, SourceProc (the whole machine), or empty when none could
	// be read, and a Sample carries only the tasks.
	Source string `json:"source"`
	// LimitCores and LimitMemory are what the container may use, and
	// LimitsFrom says whose word that is: "cgroup", "config" (what the
	// deployment says the replica is, Config.LimitCores), or "host" (every
	// CPU and all the memory the process can see).
	LimitCores  float64 `json:"limitCores,omitempty"`
	LimitMemory int64   `json:"limitMemory,omitempty"`
	LimitsFrom  string  `json:"limitsFrom,omitempty"`
	// Settings are the analysis settings in force: which models run, how many
	// workers and threads. Whatever the caller passes.
	Settings map[string]any `json:"settings,omitempty"`
}

// Sample is the container and every running task at one moment.
type Sample struct {
	Type  string       `json:"type"`
	T     time.Time    `json:"t"`
	CPU   *CPU         `json:"cpu,omitempty"`
	Mem   *Mem         `json:"mem,omitempty"`
	Tasks []TaskSample `json:"tasks"`
}

// CPU is the container's CPU since the previous sample.
type CPU struct {
	// UsedCores is CPU time over wall time: 2.5 is two and a half cores busy.
	UsedCores  float64 `json:"usedCores"`
	LimitCores float64 `json:"limitCores"`
	// ThrottledMs is how long the container was held back by its CPU limit.
	ThrottledMs int64 `json:"throttledMs"`
}

// Mem is the container's memory now, in bytes. Anon is what the processes
// actually hold; File is page cache, which the kernel reclaims before it
// kills anything, and is part of Current.
type Mem struct {
	Current  int64 `json:"current"`
	Anon     int64 `json:"anon"`
	File     int64 `json:"file"`
	Limit    int64 `json:"limit"`
	OOMKills int64 `json:"oomKills"`
}

// TaskSample is one running task at one moment, summed over the process
// groups it has started.
type TaskSample struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Phase string `json:"phase,omitempty"`
	// Cores is the task's CPU time over wall time since the previous sample.
	Cores float64 `json:"cores"`
	// PSS is proportional set size: shared pages split between the processes
	// sharing them. birdnet's workers are forked from the script and share
	// much of it, so resident set size would count that twice.
	PSS   int64 `json:"pss"`
	Procs int   `json:"procs"`
}

// Task is one model's pass over one file (or, from the benchmark, over a
// batch of them).
type Task struct {
	Type string `json:"type"`
	// T is when the task finished.
	T     time.Time `json:"t"`
	ID    string    `json:"id"`
	Model string    `json:"model"`
	Card  string    `json:"card,omitempty"`
	File  string    `json:"file,omitempty"`
	Path  string    `json:"path,omitempty"`
	// Workers and Threads are what the model was run with; Threads 0 is the
	// model's default.
	Workers int `json:"workers"`
	Threads int `json:"threads"`
	// Result is "ok", "unreadable" (the file itself), or "error" (anything
	// else), with the first line of the error.
	Result     string  `json:"result"`
	Error      string  `json:"error,omitempty"`
	AudioSec   float64 `json:"audioSec,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
	Detections int     `json:"detections"`
	// WaitSec is how long the file waited for this step: since it was
	// uploaded, for BirdNET, and since BirdNET finished it, for Perch.
	WaitSec float64 `json:"waitSec,omitempty"`
	WallSec float64 `json:"wallSec"`
	// Phases is wall time per phase: download, analyze, clip, store.
	Phases map[string]float64 `json:"phases"`
	// CPUSec is user and system time of every process the task ran, from the
	// kernel's own accounting when each exited -- exact, unlike the samples.
	CPUSec float64 `json:"cpuSec"`
	// PeakPSS is the highest PSS any sample saw, summed over the task's
	// processes. A task shorter than one sample interval has none.
	PeakPSS int64 `json:"peakPss"`
	// MaxRSS is the largest resident set of any single process the task ran,
	// from the kernel: a floor for the peak that no sampling can miss.
	MaxRSS int64 `json:"maxRss"`
	// RunningAtStart counts the other tasks running, by model, when this one
	// began: a task that shared the machine is slower than one that didn't.
	RunningAtStart map[string]int `json:"runningAtStart,omitempty"`
}
