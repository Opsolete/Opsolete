//go:build !linux

package runner

import (
	"errors"
	"os/exec"
)

// Runners are isolated on Linux (containers, the NixOS service) only; elsewhere they're for local development.
const isolates = false

func hardenProcess() error         { return nil }
func killJobProcesses() error      { return nil }
func isolateCommand(cmd *exec.Cmd) {}
func killCommand(cmd *exec.Cmd)    { cmd.Process.Kill() }

func sandboxAttrs(cmd *exec.Cmd)     {}
func probeSandbox(self string) error { return errors.New("the job sandbox needs Linux") }

// SandboxMain: the helper exists on Linux only.
func SandboxMain(args []string) int { return 125 }
