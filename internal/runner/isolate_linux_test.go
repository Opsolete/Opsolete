//go:build linux

package runner

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/config"
)

// Whatever a job leaves running, also a setsid daemon or a double fork, is gone after the job,
// whether the runner is PID 1 (container default), under an init (tini, --init) or neither (CI).
func TestJobProcessesKilled(t *testing.T) {
	if err := hardenProcess(); err != nil {
		t.Fatal(err)
	}
	marker := "OPS_TEST_LEFTOVER=" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	bin := filepath.Join(dir, "terraform")
	// ": ;" keeps sh from exec-ing sleep, so each daemon is a sh with sleep under it
	script := "#!/bin/sh\n[ \"$1\" = plan ] || exit 0\nexport " + marker + "\n" +
		"setsid sh -c 'echo $$ >>" + pids + "; sleep 300; :' </dev/null >/dev/null 2>&1 &\n" +
		"( sh -c 'echo $$ >>" + pids + "; sleep 300; :' </dev/null >/dev/null 2>&1 & )\n" +
		"while [ $(wc -l <" + pids + " 2>/dev/null || echo 0) -lt 2 ]; do sleep 0.05; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", Terraform: bin, JobTimeout: time.Minute, JobGrace: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, logs := f.do(t, r, job("leftover", "plan", "plan", "", nil))
	if res.Status != "succeeded" {
		t.Fatalf("%+v\n%s", res, logs)
	}
	if b, _ := os.ReadFile(pids); len(strings.Fields(string(b))) != 2 {
		t.Fatalf("test is void: the daemons didn't start (pids %q)", b)
	}
	environs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, e := range environs {
		if b, err := os.ReadFile(e); err == nil && bytes.Contains(b, []byte(marker)) {
			cmdline, _ := os.ReadFile(filepath.Join(filepath.Dir(e), "cmdline"))
			t.Errorf("left running after the job: %s %q", filepath.Dir(e), bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}))
		}
	}
	if kids := children(os.Getpid()); len(kids) != 0 {
		t.Errorf("children not reaped: %v", kids)
	}
}
