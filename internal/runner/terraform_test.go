package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/config"
)

func TestLostLeaseStopsTerraform(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	// An apply that would take a minute; losing the lease must interrupt it.
	src := f.module(t, `
resource "terraform_data" "slow" {
  provisioner "local-exec" {
    command = "sleep 60"
  }
}`)
	f.do(t, r, job("slow", "plan", "apply", src, nil))
	f.conflictAfter = "==> terraform apply"
	f.logs.Reset()
	f.finish = nil
	start := time.Now()
	j := job("slow", "apply", "apply", src, nil)
	f.stateToken, f.planning = j.StateToken, j.Phase == "plan"
	r.Execute(context.Background(), j)
	if f.finish != nil {
		t.Fatalf("a runner without the lease must not report a result: %+v", f.finish)
	}
	if !strings.Contains(f.logs.String(), "==> terraform apply") {
		t.Fatalf("test is void: terraform never reached apply:\n%s", f.logs.String())
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Fatalf("terraform was not interrupted (took %s)", d)
	}
}

func TestCancelSavesState(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, `
resource "terraform_data" "slow" {
  provisioner "local-exec" {
    command = "sleep 60"
  }
}`)
	f.do(t, r, job("c", "plan", "apply", src, nil))
	f.cancelAfter = "==> terraform apply"
	start := time.Now()
	res, logs := f.do(t, r, job("c", "apply", "apply", src, nil))
	if res.Status != "canceled" || !strings.Contains(res.Message, "canceled by a user") {
		t.Fatalf("canceled run: %+v\n%s", res, logs)
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Fatalf("terraform was not interrupted (took %s)", d)
	}
	// The interrupted resource exists in the cloud now: Terraform must have saved it to state, and
	// the run reports it, so nothing is orphaned.
	if !strings.Contains(string(f.states["ws1"]), "terraform_data") || len(res.Resources) != 1 || f.locks["ws1"] != "" {
		t.Fatalf("a graceful cancel must save state: resources %+v, state %q, lock %q", res.Resources, f.states["ws1"], f.locks["ws1"])
	}
}

// Every interrupt (job timeout, user cancel, runner shutdown) follows one rule: SIGINT, then at most
// OPS_JOB_GRACE before Terraform is killed. The fake terraform's plan either ignores SIGINT or stops
// shortly after it.
func TestInterrupt(t *testing.T) {
	const timeout, grace = 300 * time.Millisecond, 400 * time.Millisecond
	ignore := "trap '' INT; exec sleep 10"
	stop := "trap 'sleep 0.1; kill $!; exit 1' INT; sleep 10 >/dev/null & wait"
	for _, c := range []struct {
		name, plan, cause, want string
		killed                  bool
	}{
		{"timeout between commands", ignore, "stall", "timed out after 300ms; no Terraform command was running at the time", false},
		{"timeout killed", ignore, "timeout", "timed out after 300ms; Terraform was interrupted but didn't stop within OPS_JOB_GRACE (400ms) and was killed", true},
		{"timeout clean", stop, "timeout", "timed out after 300ms; Terraform was interrupted and saved its state", false},
		{"cancel killed", ignore, "cancel", "canceled by a user; Terraform was interrupted but didn't stop", true},
		{"cancel clean", stop, "cancel", "canceled by a user; Terraform was interrupted and saved its state", false},
		{"shutdown killed", ignore, "shutdown", "runner was shut down during the run; Terraform was interrupted but didn't stop", true},
		{"shutdown clean", stop, "shutdown", "runner was shut down during the run; Terraform was interrupted and saved its state", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "terraform")
			script := "#!/bin/sh\n[ \"$1\" = plan ] || exit 0\necho planning\n" + c.plan + "\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			f := &fakeServer{}
			srv := httptest.NewServer(f)
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", Terraform: bin, JobTimeout: time.Minute, JobGrace: grace},
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			hard, interrupt := context.WithCancel(context.Background())
			defer interrupt()
			phase := "plan"
			switch c.cause {
			case "timeout":
				r.JobTimeout = timeout
			case "stall": // the apply phase's plan download hangs past the deadline, before any terraform command
				r.JobTimeout, f.stall, phase = timeout, true, "apply"
			case "cancel":
				f.cancelAfter = "planning" // the first log post, about a second in
			case "shutdown":
				time.AfterFunc(timeout, interrupt)
			}
			start := time.Now()
			r.Execute(hard, job("i", phase, "plan", "", nil))
			took := time.Since(start)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.finish == nil || f.finish.Status == "succeeded" || !strings.HasPrefix(f.finish.Message, c.want) {
				t.Fatalf("result %+v, want message %q\n%s", f.finish, c.want, f.logs.String())
			}
			// interrupted by about 1.2s at the latest (the cancel arrives with a log post)
			if limit := 1500*time.Millisecond + grace; took > limit || c.killed && took < timeout+grace {
				t.Fatalf("took %s: want at most %s, and killed only after the grace", took, limit)
			}
		})
	}
}

func TestHeadBuffer(t *testing.T) {
	h := headBuffer{max: 5}
	for _, s := range []string{"abc", "def", "ghi"} {
		if n, err := h.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v: must report all written, or MultiWriter stops", s, n, err)
		}
	}
	if h.String() != "abcde" {
		t.Fatalf("kept %q, want abcde", h.String())
	}
}
