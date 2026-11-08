//go:build !linux

package runner

import "os/exec"

// Runners are isolated on Linux (containers) only; elsewhere they're for local development.
func hardenProcess() error         { return nil }
func killJobProcesses()            {}
func isolateCommand(cmd *exec.Cmd) {}
func killCommand(cmd *exec.Cmd)    { cmd.Process.Kill() }
