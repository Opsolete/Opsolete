package runner

import (
	"os"
	"testing"
)

// The test binary is the runner's binary in the sandbox tests: it runs the helper as cmd/opsolete does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SandboxArg {
		os.Exit(SandboxMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// sandboxed is the test binary as the sandbox's helper; the test skips where user namespaces are forbidden
// (a container's default seccomp profile) unless OPS_TEST_SANDBOX=1 requires them.
func sandboxed(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := probeSandbox(self); err != nil {
		if os.Getenv("OPS_TEST_SANDBOX") == "1" {
			t.Fatalf("sandbox required (OPS_TEST_SANDBOX=1): %v", err)
		}
		t.Skipf("no sandbox here: %v", err)
	}
	return self
}
