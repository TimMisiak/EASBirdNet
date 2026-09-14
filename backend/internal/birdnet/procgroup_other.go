//go:build !unix

package birdnet

import "os/exec"

// killProcessGroupOnCancel leaves exec's default, killing just the script.
// Birdsense runs on Linux; this only keeps the package building elsewhere.
func killProcessGroupOnCancel(*exec.Cmd) {}
