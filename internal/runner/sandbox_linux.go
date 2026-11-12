//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// The job sandbox (OPS_SANDBOX): every Terraform command runs in its own user, mount, PID and IPC namespaces. The
// runner starts itself as a helper (SandboxArg), which starts itself again in them (nsArg): not the runner directly,
// since the runner isn't dumpable (its memory holds the token), so neither is a child it clones before that child's
// exec, and the child's uid_map can't be written then. The helper's exec made it dumpable again; it holds only the
// job's environment, forwards interrupts and exits as the command does. In the namespaces it makes /tmp a private tmpfs with only the
// job's directories bound back in, mounts its own /proc where the kernel lets it, and execs Terraform, which is then
// PID 1 of its namespace: when it exits the kernel kills whatever it left running. Inside, the runner's user is root
// of the user namespace and nobody else; outside it's the runner's user, as before. A job then doesn't see another
// job's temporary files, the runner's or the other processes, and leaves nothing behind in /tmp.

// nsArg starts the helper's second stage, in the namespaces.
const nsArg = "--ns"

// sandboxAttrs are the namespaces' process attributes; it dies with the helper.
func sandboxAttrs(cmd *exec.Cmd) {
	a := &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.SysProcAttr = a
	a.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC
	a.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
	a.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	a.GidMappingsEnableSetgroups = false
}

// probeSandbox reports whether the sandbox works here: the helper sets up a job's namespaces and mounts (dir as
// the job's directory) and exits. The error says why not (a kernel or container that forbids user namespaces).
func probeSandbox(self string) error {
	dir, err := os.MkdirTemp("", "ops-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(self, SandboxArg, "--bind", dir, "--probe")
	isolateCommand(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat(filepath.Join(dir, "probe")); err != nil {
		return errors.New("the job's directory isn't writable in the sandbox")
	}
	return nil
}

// SandboxMain is the helper: it runs its second stage in new namespaces and returns its exit status (125: the
// sandbox failed, the reason on stderr; 128+n: killed by signal n).
func SandboxMain(args []string) int {
	if len(args) > 0 && args[0] == nsArg {
		return sandboxStage(args[1:])
	}
	runtime.LockOSThread() // Pdeathsig: the stage dies with this thread, which lives as long as the helper
	cmd := exec.Command("/proc/self/exe", append([]string{SandboxArg, nsArg}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sandboxAttrs(cmd)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM) // the runner's interrupt: Terraform saves its state
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: user namespaces: %v\n", err)
		return 125
	}
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()
	cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// sandboxStage is the helper's second stage, in the new namespaces: it returns only on failure (exit status 125, the
// reason on stderr); otherwise it becomes the command.
func sandboxStage(args []string) int {
	var binds, cmd []string
	probe := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--bind":
			if i+1 < len(args) {
				binds = append(binds, args[i+1])
				i++
			}
		case "--probe":
			probe = true
		case "--":
			cmd = args[i+1:]
			i = len(args)
		}
	}
	fail := func(what string, err error) int {
		fmt.Fprintf(os.Stderr, "sandbox: %s: %v\n", what, err)
		return 125
	}
	if !probe && len(cmd) == 0 {
		return fail("usage", errors.New("no command"))
	}
	// nothing mounted here reaches the runner's namespace
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fail("make mounts private", err)
	}
	var bin string
	if !probe {
		var err error
		if bin, err = exec.LookPath(cmd[0]); err != nil {
			return fail("find "+cmd[0], err)
		}
		if strings.HasPrefix(bin, "/tmp/") { // a terraform under /tmp stays reachable (tests, a downloaded binary)
			binds = append(binds, filepath.Dir(bin))
		}
	}
	// the job's directories, by handle: the private /tmp hides their paths
	fds := make([]*os.File, len(binds))
	for i, b := range binds {
		f, err := os.Open(b)
		if err != nil {
			return fail("open "+b, err)
		}
		fds[i] = f
	}
	wd, _ := os.Getwd() // the job's directory: the private /tmp hides it until it's bound back
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=1777"); err != nil {
		return fail("private /tmp", err)
	}
	for i, b := range binds {
		if err := os.MkdirAll(b, 0o700); err != nil {
			return fail("bind "+b, err)
		}
		if err := syscall.Mount(fmt.Sprintf("/proc/self/fd/%d", fds[i].Fd()), b, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fail("bind "+b, err)
		}
		fds[i].Close()
	}
	// its own /proc: only its processes. A container that masks parts of /proc refuses this; the runner's
	// memory and environment stay unreadable anyway (it's not dumpable).
	syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "")
	if wd != "" && os.Chdir(wd) != nil { // a command's is a job directory; elsewhere under /tmp it's gone
		os.Chdir("/")
	}
	if probe {
		if len(binds) == 0 {
			return 0
		}
		if err := os.WriteFile(filepath.Join(binds[0], "probe"), nil, 0o600); err != nil {
			return fail("write the job's directory", err)
		}
		return 0
	}
	return fail("exec "+bin, syscall.Exec(bin, cmd, os.Environ()))
}
