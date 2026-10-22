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
	conflictAfter string            // answer log posts with 409 (lease lost) once the log contains this
	states        map[string][]byte // http state backend: workspace id -> state
	locks         map[string]string // workspace id -> lock id
	artifacts     map[string][]byte // "<run>/<name>" -> saved plan / lock file
	stateToken    string            // the state password of the job being executed
	cancelAfter   string            // answer log posts with 202 (user canceled) once the log contains this
}

// artifact stores and serves plan artifacts, like the server, only for the job's lease.
func (f *fakeServer) artifact(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(runnerapi.LeaseHeader) != "lease" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, runnerapi.Prefix+"/jobs/")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.artifacts == nil {
		f.artifacts = map[string][]byte{}
	}
	if r.Method == http.MethodPost {
		f.artifacts[key], _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b, ok := f.artifacts[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write(b)
}

// state is a minimal Terraform http state backend with the server's rules: per-run credentials,
// and writes only under the lock.
func (f *fakeServer) state(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, runnerapi.StatePrefix+"/")
	// Like the server: one password per run, accepted only for the run being executed.
	if u, p, ok := r.BasicAuth(); !ok || u != id || p != f.stateToken {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states, f.locks = map[string][]byte{}, map[string]string{}
	}
	body, _ := io.ReadAll(r.Body)
	var info struct{ ID string }
	json.Unmarshal(body, &info)
	switch r.Method {
	case "GET":
		if b, ok := f.states[id]; ok {
			w.Write(b)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "POST":
		if f.locks[id] == "" || r.URL.Query().Get("ID") != f.locks[id] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.states[id] = body
	case "LOCK":
		if f.locks[id] != "" {
			w.WriteHeader(http.StatusLocked)
			return
		}
		f.locks[id] = info.ID
	case "UNLOCK":
		delete(f.locks, id)
	}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, runnerapi.StatePrefix+"/") {
		f.state(w, r)
		return
	}
	if strings.Contains(r.URL.Path, "/artifacts/") {
		f.artifact(w, r)
		return
	}
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
		if f.cancelAfter != "" && strings.Contains(f.logs.String(), f.cancelAfter) {
			w.WriteHeader(runnerapi.StatusCancel)
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
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", Terraform: tf, JobTimeout: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// job is one phase of run (on workspace "ws1") with the fake server's credentials.
func job(run, phase, action, src string, vars map[string]any) runnerapi.Job {
	return runnerapi.Job{RunID: run, WorkspaceID: "ws1", Phase: phase, Action: action, LeaseID: "lease", LeaseSeconds: 120,
		ModuleSource: src, StateToken: "state-tok-" + run, Vars: vars}
}

// do runs one job and returns what it reported, plus its log.
func (f *fakeServer) do(t *testing.T, r *Runner, j runnerapi.Job) (*runnerapi.FinishRequest, string) {
	t.Helper()
	f.logs.Reset()
	f.finish = nil
	f.stateToken = j.StateToken
	r.Execute(context.Background(), j)
	if f.finish == nil {
		t.Fatalf("%s %s: no result\n%s", j.Phase, j.RunID, f.logs.String())
	}
	return f.finish, f.logs.String()
}

const helloModule = `
variable "greeting" { type = string }
resource "terraform_data" "this" { input = var.greeting }
output "msg" { value = terraform_data.this.output }
output "secret" {
  value     = "hunter2"
  sensitive = true
}`

func TestPlanThenApply(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, helloModule)
	vars := map[string]any{"greeting": "hello"}

	// Plan phase: nothing is applied; the plan is summarized and saved on the server.
	res, logs := f.do(t, r, job("run1", "plan", "apply", src, vars))
	want := runnerapi.Plan{Add: 1, Changes: []runnerapi.Change{{Address: "terraform_data.this", Action: "create"}}}
	if res.Status != "succeeded" || res.Plan == nil || res.Plan.Add != want.Add || len(res.Plan.Changes) != 1 || res.Plan.Changes[0] != want.Changes[0] {
		t.Fatalf("plan: %+v\n%s", res, logs)
	}
	if res.Resources != nil || f.states["ws1"] != nil || len(f.artifacts["run1/artifacts/tfplan"]) == 0 {
		t.Fatalf("a plan must not touch state, and must save the plan: %+v %q", res.Resources, f.states["ws1"])
	}
	if !strings.Contains(logs, "plan saved: 1 to add, 0 to change, 0 to destroy") || strings.Contains(logs, `"resource_changes"`) {
		t.Fatalf("plan log (the plan's JSON must not be streamed):\n%s", logs)
	}

	// Apply phase: applies exactly the saved plan.
	res, logs = f.do(t, r, job("run1", "apply", "apply", src, vars))
	if res.Status != "succeeded" || res.Outputs["msg"] != "hello" || res.Outputs["secret"] != "(sensitive)" {
		t.Fatalf("apply: %+v\n%s", res, logs)
	}
	wantRes := runnerapi.Resource{Address: "terraform_data.this", Type: "terraform_data", Name: "this", Provider: "terraform.io/builtin/terraform"}
	if len(res.Resources) != 1 || res.Resources[0] != wantRes {
		t.Fatalf("resources: %+v", res.Resources)
	}
	for _, leak := range []string{"hunter2", "secret-signature", `"values"`, "state-tok-"} {
		if strings.Contains(logs, leak) {
			t.Errorf("%q leaked into the logs:\n%s", leak, logs)
		}
	}
	if !strings.Contains(string(f.states["ws1"]), `"terraform_data"`) || f.locks["ws1"] != "" {
		t.Fatalf("state not saved to the server, or lock not released: %q %q", f.states["ws1"], f.locks["ws1"])
	}

	// Same inputs again: nothing to do.
	if res, logs = f.do(t, r, job("run2", "plan", "apply", src, vars)); res.Status != "succeeded" || res.Plan != nil {
		t.Fatalf("second plan should find no changes: %+v\n%s", res, logs)
	}

	// A plan made before the state changed is refused, and the failed apply still reports the
	// state, without streaming it.
	f.do(t, r, job("stale", "plan", "apply", src, map[string]any{"greeting": "hi"}))
	f.do(t, r, job("fresh", "plan", "apply", src, map[string]any{"greeting": "hey"}))
	if res, _ := f.do(t, r, job("fresh", "apply", "apply", src, map[string]any{"greeting": "hey"})); res.Status != "succeeded" {
		t.Fatalf("fresh apply: %+v", res)
	}
	res, logs = f.do(t, r, job("stale", "apply", "apply", src, map[string]any{"greeting": "hi"}))
	if res.Status != "failed" || !strings.HasPrefix(res.Message, "apply: ") || len(res.Resources) != 1 {
		t.Fatalf("stale plan must be refused: %+v\n%s", res, logs)
	}
	if strings.Contains(logs, `"values"`) || strings.Contains(logs, "hunter2") {
		t.Fatalf("state leaked into the log of a failed apply:\n%s", logs)
	}

	// Destroy: planned, then applied; the state ends up empty, reported as such.
	if res, _ = f.do(t, r, job("d", "plan", "destroy", src, vars)); res.Plan == nil || res.Plan.Destroy != 1 || res.Plan.Changes[0].Action != "delete" {
		t.Fatalf("destroy plan: %+v", res.Plan)
	}
	res, logs = f.do(t, r, job("d", "apply", "destroy", src, vars))
	if res.Status != "succeeded" || res.Outputs != nil || !strings.Contains(logs, "0 added, 0 changed, 1 destroyed") {
		t.Fatalf("destroy: %+v\n%s", res, logs)
	}
	if res.Resources == nil || len(res.Resources) != 0 {
		t.Fatalf("after destroy the inventory must be reported empty, not unknown: %#v", res.Resources)
	}
}

func TestExecuteReportsFailure(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, `variable "n" { type = number }`)
	if res, _ := f.do(t, r, job("bad", "plan", "apply", src, map[string]any{"n": "not a number"})); res.Status != "failed" || !strings.HasPrefix(res.Message, "plan: ") {
		t.Fatalf("finish: %+v", res)
	}
	if res, _ := f.do(t, r, job("nosaved", "apply", "apply", src, nil)); res.Status != "failed" || !strings.HasPrefix(res.Message, "download plan: ") {
		t.Fatalf("apply without a saved plan: %+v", res)
	}
}

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
	f.stateToken = j.StateToken
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
