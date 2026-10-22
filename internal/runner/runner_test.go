package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rudesome/form2stack/internal/config"
	"github.com/rudesome/form2stack/internal/registry/registrytest"
	"github.com/rudesome/form2stack/internal/runnerapi"
)

// fakeServer records what a runner sends; conflictLogs makes it answer 409 (lease lost).
type fakeServer struct {
	modules       map[string][]byte // archive path -> .tar.gz, served without auth like signed URLs
	mu            sync.Mutex
	logs          strings.Builder
	finish        *runnerapi.FinishRequest
	conflictAfter string // answer log posts with 409 (lease lost) once the log contains this
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if b, ok := f.modules[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(runnerapi.LeaseHeader) != "lease" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/logs"):
		f.logs.Write(body)
		if f.conflictAfter != "" && strings.Contains(f.logs.String(), f.conflictAfter) {
			w.WriteHeader(http.StatusConflict)
			return
		}
	case strings.HasSuffix(r.URL.Path, "/finish"):
		var req runnerapi.FinishRequest
		json.Unmarshal(body, &req)
		f.finish = &req
	}
	w.WriteHeader(http.StatusNoContent)
}

func testRunner(t *testing.T, srv *httptest.Server) *Runner {
	tf := os.Getenv("TERRAFORM")
	if tf == "" {
		var err error
		if tf, err = exec.LookPath("terraform"); err != nil {
			t.Skip("terraform not installed")
		}
	}
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", Terraform: tf, Backend: "local",
		StateDir: t.TempDir(), JobTimeout: time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

// module serves tf as a registry-style archive and returns the server-relative source the
// server would send ("/..." is resolved against the runner's server URL).
func (f *fakeServer) module(t *testing.T, tf string) string {
	path := "/v1/archives/" + strings.ReplaceAll(t.Name(), "/", "_") + ".tar.gz"
	if f.modules == nil {
		f.modules = map[string][]byte{}
	}
	f.modules[path] = registrytest.TarGz(t, map[string]string{"main.tf": tf})
	return path + "?expires=1&sig=secret-signature"
}

func TestExecuteApplies(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, `
variable "greeting" { type = string }
resource "terraform_data" "this" { input = var.greeting }
output "msg" { value = terraform_data.this.output }
output "secret" {
  value     = "hunter2"
  sensitive = true
}`)
	job := runnerapi.Job{DeploymentID: "d1", LeaseID: "lease", LeaseSeconds: 120, ModuleSource: src,
		StateKey: "form2stack/t/d1.tfstate", Vars: map[string]any{"greeting": "hello"}}
	r.Execute(context.Background(), job)

	if f.finish == nil || f.finish.Status != "succeeded" {
		t.Fatalf("finish: %+v\nlogs:\n%s", f.finish, f.logs.String())
	}
	if f.finish.Outputs["msg"] != "hello" || f.finish.Outputs["secret"] != "(sensitive)" {
		t.Fatalf("outputs: %+v", f.finish.Outputs)
	}
	logs := f.logs.String()
	for _, want := range []string{"==> terraform plan", "Apply complete! Resources: 1 added", "==> done"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "hunter2") || strings.Contains(logs, "secret-signature") {
		t.Errorf("sensitive value or signed URL leaked into logs:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir, job.StateKey)); err != nil {
		t.Fatalf("state not written to the deployment's own key: %v", err)
	}

	// Same deployment again: state is reused, so nothing changes.
	f.logs.Reset()
	r.Execute(context.Background(), job)
	if f.finish.Status != "succeeded" || !strings.Contains(f.logs.String(), "no changes") {
		t.Fatalf("second run should be a no-op:\n%s", f.logs.String())
	}

	// Destroy tears down what's in the deployment's state.
	f.logs.Reset()
	job.Action = "destroy"
	r.Execute(context.Background(), job)
	if f.finish.Status != "succeeded" || f.finish.Outputs != nil || !strings.Contains(f.logs.String(), "Apply complete! Resources: 0 added, 0 changed, 1 destroyed") {
		t.Fatalf("destroy: %+v\n%s", f.finish, f.logs.String())
	}
}

func TestExecuteReportsFailure(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, `variable "n" { type = number }`)
	r.Execute(context.Background(), runnerapi.Job{DeploymentID: "d2", LeaseID: "lease", LeaseSeconds: 120,
		ModuleSource: src, StateKey: "s/d2.tfstate", Vars: map[string]any{"n": "not a number"}})
	if f.finish == nil || f.finish.Status != "failed" || !strings.HasPrefix(f.finish.Message, "plan: ") {
		t.Fatalf("finish: %+v", f.finish)
	}
}

func TestLostLeaseStopsTerraform(t *testing.T) {
	f := &fakeServer{conflictAfter: "==> terraform apply"}
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
	start := time.Now()
	r.Execute(context.Background(), runnerapi.Job{DeploymentID: "d3", LeaseID: "lease", LeaseSeconds: 120,
		ModuleSource: src, StateKey: "s/d3.tfstate"})
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
