//go:build linux

package runner

import (
	"os"
	"syscall"
)

// hardenProcess makes the runner non-dumpable, so the jobs it runs as the same user can't read its
// /proc/<pid>/environ or memory: those still hold OPS_RUNNER_TOKEN, os.Unsetenv doesn't clear them.
func hardenProcess() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return e
	}
	return nil
}

// killJobProcesses kills whatever a job left running (a backgrounded local-exec, say), so it can't
// read the next job's credentials, and reaps it. Only as a container's PID 1: kill(-1) signals every
// process of the user except the caller, which outside a container would be the whole session.
func killJobProcesses() {
	if os.Getpid() != 1 {
		return
	}
	syscall.Kill(-1, syscall.SIGKILL)
	for {
		_, err := syscall.Wait4(-1, nil, 0, nil)
		if err != syscall.EINTR && err != nil { // ECHILD: all reaped
			return
		}
	}
}
