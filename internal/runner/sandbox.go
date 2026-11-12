package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// SandboxArg is the subcommand that runs the job sandbox's setup helper (sandbox_linux.go):
// `opsolete __sandbox [--bind dir]… [--probe | -- cmd args…]`.
const SandboxArg = "__sandbox"

// sandboxCommand is the helper running bin args in new namespaces, binds (the job's directories) kept writable.
func sandboxCommand(ctx context.Context, self, bin string, args, binds []string) *exec.Cmd {
	argv := []string{SandboxArg}
	for _, b := range binds {
		argv = append(argv, "--bind", b)
	}
	argv = append(append(argv, "--", bin), args...)
	return exec.CommandContext(ctx, self, argv...)
}

// setupSandbox decides whether jobs run in the sandbox (r.Sandbox: namespaces requires it, auto uses it if it
// works here, off or "" never): it probes it, and remembers the runner's binary to start the helper from.
func (r *Runner) setupSandbox() error {
	if r.Sandbox == "" || r.Sandbox == "off" {
		r.Log.Warn("job sandbox off (OPS_SANDBOX=off): jobs share the runner's /tmp and see its processes")
		return nil
	}
	self, err := os.Executable()
	if err == nil {
		err = probeSandbox(self)
	}
	switch {
	case err == nil:
		r.self = self
		r.Log.Info("job sandbox: namespaces (private /tmp, own processes)")
	case r.Sandbox == "namespaces":
		return fmt.Errorf("OPS_SANDBOX=namespaces, but the sandbox doesn't work here: %w", err)
	default:
		r.Log.Warn("job sandbox unavailable here (OPS_SANDBOX=auto): jobs share the runner's /tmp and see its processes", "err", err)
	}
	return nil
}
