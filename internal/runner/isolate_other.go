//go:build !linux

package runner

// Runners are isolated on Linux (containers) only; elsewhere they're for local development.
func hardenProcess() error { return nil }
func killJobProcesses()    {}
