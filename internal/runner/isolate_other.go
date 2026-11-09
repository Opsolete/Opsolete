//go:build !linux

package runner

import "os/exec"

// Runners are isolated on Linux (containers, the NixOS service) only; elsewhere they're for local development.
const isolates = false

func hardenProcess() error         { return nil }
func killJobProcesses() error      { return nil }
func isolateCommand(cmd *exec.Cmd) {}
func killCommand(cmd *exec.Cmd)    { cmd.Process.Kill() }
