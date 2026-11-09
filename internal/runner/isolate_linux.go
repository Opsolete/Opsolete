//go:build linux

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// isolates: on Linux every process a job leaves behind is killed before the next job.
const isolates = true

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>; the frozen syscall package
// lacks it.
const prSetChildSubreaper = 36

// isolateCommand gives terraform what terraform-exec does on Linux: killed if the runner dies, and
// its own process group.
func isolateCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

// hardenProcess makes the runner non-dumpable, so the jobs it runs as the same user can't read its
// /proc/<pid>/environ or memory: those still hold OPS_RUNNER_TOKEN, os.Unsetenv doesn't clear them.
// And a child subreaper: whatever a job leaves running (also double-forked or setsid daemons)
// re-parents to the runner instead of init, so killJobProcesses finds it, PID 1 or not.
func hardenProcess() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return fmt.Errorf("PR_SET_DUMPABLE: %w", e)
	}
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return fmt.Errorf("PR_SET_CHILD_SUBREAPER: %w", e)
	}
	return nil
}

// killJobProcesses kills and reaps every child of the runner, so nothing a job left running (a
// backgrounded local-exec, a daemonized provider) can read the next job's credentials. Called only
// between jobs, when the runner has no children of its own (Terraform and infracost have exited).
// Grandchildren re-parent to the runner when their parent dies, so it repeats until none are left.
// As a container's PID 1 this also reaps the container's other orphans.
func killJobProcesses() error {
	self := os.Getpid()
	deadline := time.Now().Add(5 * time.Second)
	for {
		kids := children(self)
		for _, pid := range kids {
			syscall.Kill(-pid, syscall.SIGKILL) // its process group, if it leads one
			syscall.Kill(pid, syscall.SIGKILL)
		}
		for {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if pid <= 0 && err != syscall.EINTR {
				break // none exited yet, or ECHILD
			}
		}
		if len(kids) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %v survived SIGKILL", kids)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// children lists the processes whose parent is ppid, zombies included.
func children(ppid int) []int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var pids []int
	for _, f := range stats {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // exited meanwhile
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parentheses
		fields := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(ppid) {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(f))); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killCommand kills a terraform command's whole process group: Terraform, its providers and what
// provisioners started.
func killCommand(cmd *exec.Cmd) { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
