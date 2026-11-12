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

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
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
	planning      bool              // the job is a plan phase: like the server, refuse state writes
	cancelAfter   string            // answer log posts with 202 (user canceled) once the log contains this
	claim         http.HandlerFunc  // answers POST /jobs/claim
	calls         []string          // job calls in order: ack, logs, heartbeat, finish
	stall         bool              // artifact downloads hang until the runner gives up
	failWrites    int               // refuse state writes under this many locks (one per terraform command)
	refused       bool              // a write under the current lock was refused
}

// artifact stores and serves plan artifacts, like the server, only for the job's lease.
func (f *fakeServer) artifact(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(runnerapi.LeaseHeader) != "lease" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, runnerapi.Prefix+"/jobs/")
	if f.stall && r.Method == http.MethodGet {
		<-r.Context().Done()
		return
	}
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
		if f.failWrites > 0 {
			f.refused = true
			w.WriteHeader(http.StatusConflict)
			return
		}
		if f.planning || f.locks[id] == "" || r.URL.Query().Get("ID") != f.locks[id] {
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
		if f.refused {
			f.failWrites, f.refused = f.failWrites-1, false
		}
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
	if r.URL.Path == runnerapi.Prefix+"/jobs/claim" {
		f.claim(w, r)
		return
	}
	if r.Method == http.MethodGet {
		f.mu.Lock()
		b, ok := f.modules[r.URL.Path]
		f.mu.Unlock()
		if ok {
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
	f.calls = append(f.calls, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
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
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", Terraform: tf, JobTimeout: time.Minute, JobGrace: time.Minute},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

// module serves tf as a registry-style archive and returns the server-relative source the
// server would send ("/..." is resolved against the runner's server URL).
func (f *fakeServer) module(t *testing.T, tf string) string {
	path := "/v1/archives/" + strings.ReplaceAll(t.Name(), "/", "_") + ".tar.gz"
	f.mu.Lock() // the server may be serving the previous job's archive
	defer f.mu.Unlock()
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
	f.stateToken, f.planning = j.StateToken, j.Phase == "plan"
	if os.Getenv("OPS_TEST_SANDBOX") == "1" && r.self == "" { // test-linux's sandbox pass: every job in it
		r.self = sandboxed(t)
	}
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
