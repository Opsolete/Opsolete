//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// In the sandbox a command is PID 1 of its own namespace, sees a private /tmp (not the runner's files, its own
// leave nothing behind) with the job's directories in it, and what it leaves running dies with it.
func TestSandbox(t *testing.T) {
	if err := hardenProcess(); err != nil { // as the runner: not dumpable (its child's uid_map then isn't writable)
		t.Fatal(err)
	}
	self := sandboxed(t)
	hostFile := filepath.Join(os.TempDir(), "ops-sandbox-host-"+time.Now().Format("150405.000000"))
	if err := os.WriteFile(hostFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(hostFile)
	dir, home := t.TempDir(), t.TempDir() // under /tmp, like a job's
	leftover := "/tmp/ops-sandbox-left-" + time.Now().Format("150405.000000")
	script := `echo pid=$$
[ -e ` + hostFile + ` ] && echo host-visible
touch ` + leftover + ` && echo wrote-tmp
pwd
echo ok > job && echo ok > ` + home + `/h
sleep 300 &
`
	marker := "OPS_TEST_SANDBOX_LEFT=" + filepath.Base(leftover)
	p := &tfProc{bin: "/bin/sh", dir: dir, env: append(os.Environ(), marker), grace: time.Second, end: time.Now().Add(time.Minute), self: self, binds: []string{dir, home}}
	var out, errs bytes.Buffer
	if _, err := p.run(context.Background(), &out, &errs, "-c", script); err != nil {
		t.Fatalf("%v\n%s%s", err, out.String(), errs.String())
	}
	got := out.String()
	for _, want := range []string{"pid=1\n", "wrote-tmp\n", dir + "\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s%s", want, got, errs.String())
		}
	}
	if strings.Contains(got, "host-visible") {
		t.Error("the runner's /tmp is visible in the sandbox")
	}
	if _, err := os.Stat(leftover); err == nil {
		os.Remove(leftover)
		t.Error("a file written to the sandbox's /tmp is in the runner's")
	}
	for _, f := range []string{filepath.Join(dir, "job"), filepath.Join(home, "h")} {
		if b, err := os.ReadFile(f); err != nil || string(b) != "ok\n" {
			t.Errorf("job directory not writable: %s %q %v", f, b, err)
		}
	}
	time.Sleep(100 * time.Millisecond) // the kernel kills the namespace's processes as its PID 1 exits
	environs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, e := range environs {
		if b, err := os.ReadFile(e); err == nil && bytes.Contains(b, []byte(marker)) {
			t.Errorf("left running after the sandbox's command: %s", filepath.Dir(e))
		}
	}
}
