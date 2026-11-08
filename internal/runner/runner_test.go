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
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

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
	if !strings.Contains(logs, "\x1b[32m+\x1b[0m") { // Terraform's own colors, for the run page
		t.Fatalf("plan log without Terraform's colors:\n%q", logs)
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

	// Drift check: a refresh-only plan; nothing drifted, and an assessment never keeps a plan.
	// It takes no state lock, so it runs while someone holds one (a locked workspace).
	f.locks["ws1"] = "someone-else"
	if res, logs = f.do(t, r, job("assess", "plan", "assess", src, vars)); res.Status != "succeeded" || res.Plan != nil ||
		!strings.Contains(logs, "plan -refresh-only") || f.artifacts["assess/artifacts/tfplan"] != nil {
		t.Fatalf("assess: %+v\n%s", res, logs)
	}
	delete(f.locks, "ws1")

	// Replace: same inputs, but the resource is planned for replacement.
	rj := job("replace", "plan", "apply", src, vars)
	rj.Replace = []string{"terraform_data.this"}
	if res, logs = f.do(t, r, rj); res.Plan == nil || len(res.Plan.Changes) != 1 || res.Plan.Changes[0].Action != "replace" {
		t.Fatalf("replace: %+v\n%s", res, logs)
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
	if res, _ := f.do(t, r, job("bad", "plan", "apply", src, map[string]any{"n": "not a number"})); res.Status != "failed" ||
		!strings.HasPrefix(res.Message, "plan: ") || !strings.Contains(res.Message, "Error") || strings.Contains(res.Message, "\x1b") {
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

// TestJobEnv: a job's environment variables reach Terraform, on top of the runner's own.
func TestJobEnv(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	j := job("env1", "plan", "apply", f.module(t, helloModule), map[string]any{"greeting": "hi"})
	data := filepath.Join(t.TempDir(), "tfdata")
	j.Env = map[string]string{"TF_DATA_DIR": data}
	if res, logs := f.do(t, r, j); res.Status != "succeeded" {
		t.Fatalf("plan: %+v\n%s", res, logs)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("TF_DATA_DIR from the job env not used: %v", err)
	}
}

// TestCompactWarnings: warnings are one line each, unless the workspace sets OPS_DEBUG.
func TestCompactWarnings(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, helloModule)
	for _, debug := range []string{"", "1"} {
		j := job("warn"+debug, "plan", "plan", src, map[string]any{"greeting": "hi"})
		j.Target, j.Env = []string{"terraform_data.this"}, map[string]string{"OPS_DEBUG": debug} // -target always warns
		res, logs := f.do(t, r, j)
		if res.Status != "succeeded" || !strings.Contains(logs, "Resource targeting is in effect") ||
			strings.Contains(logs, "not for routine use") != (debug == "1") {
			t.Fatalf("OPS_DEBUG=%q: %+v\n%s", debug, res, logs)
		}
	}
}

// TestAddedTemplates: templates added to a workspace become module blocks, with or without a root
// module, their outputs root outputs, and form values stay values.
// TestEscapeValue: nested strings and object keys are escaped too, so list/object inputs can't inject.
func TestEscapeValue(t *testing.T) {
	got := escapeValue(map[string]any{"${k}": []any{"%{x}", 1, map[string]any{"a": "${file(\"/etc/passwd\")}"}}})
	want := map[string]any{"$${k}": []any{"%%{x}", 1, map[string]any{"a": "$${file(\"/etc/passwd\")}"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestAddedTemplates(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, helloModule)
	evil := `${file("/etc/passwd")}`
	mods := []runnerapi.Module{
		{Name: "a", Source: src, Inputs: map[string]any{"greeting": evil}},
		{Name: "b-2", Source: src, Inputs: map[string]any{"greeting": "hi"}},
	}
	j := job("m1", "plan", "apply", "", nil)
	j.Modules = mods
	res, logs := f.do(t, r, j)
	if res.Status != "succeeded" || res.Plan == nil || res.Plan.Add != 2 {
		t.Fatalf("plan: %+v\n%s", res, logs)
	}
	j.Phase = "apply"
	res, logs = f.do(t, r, j)
	if res.Status != "succeeded" || res.Outputs["a__msg"] != evil || res.Outputs["b-2__msg"] != "hi" || res.Outputs["a__secret"] != "(sensitive)" {
		t.Fatalf("apply: %+v\n%s", res, logs)
	}
	if len(res.Resources) != 2 || res.Resources[0].Address != "module.a.terraform_data.this" {
		t.Fatalf("resources: %+v", res.Resources)
	}
	if strings.Contains(logs, "secret-signature") || strings.Contains(logs, "hunter2") {
		t.Fatalf("leaked into the logs:\n%s", logs)
	}

	// Next to a root module, and with one template removed: its resources are destroyed.
	root := job("m2", "plan", "apply", src, map[string]any{"greeting": "root"})
	root.Modules = mods[:1]
	if res, logs = f.do(t, r, root); res.Status != "succeeded" || res.Plan == nil || res.Plan.Add != 1 || res.Plan.Destroy != 1 {
		t.Fatalf("plan with root module: %+v\n%s", res, logs)
	}

	// Removing a template: the destroy limited to it also drops its outputs, which Terraform keeps.
	d := job("m3", "plan", "destroy", "", nil)
	d.Modules, d.Target = mods, []string{"module.a"}
	if res, logs = f.do(t, r, d); res.Status != "succeeded" || res.Plan == nil || res.Plan.Destroy != 1 {
		t.Fatalf("destroy plan: %+v\n%s", res, logs)
	}
	d.Phase = "apply"
	res, logs = f.do(t, r, d)
	var st struct{ Outputs map[string]any }
	json.Unmarshal(f.states["ws1"], &st)
	if _, kept := st.Outputs["a__msg"]; res.Status != "succeeded" || kept || st.Outputs["b-2__msg"] == nil || len(res.Resources) != 1 ||
		!strings.Contains(logs, "terraform apply (outputs only)") || strings.Contains(logs, "hunter2") {
		t.Fatalf("destroy apply: %+v, outputs %v\n%s", res, st.Outputs, logs)
	}
	// the last one: no configuration is left
	d = job("m4", "plan", "destroy", "", nil)
	d.Modules, d.Target = mods[1:], []string{"module.b-2"}
	f.do(t, r, d)
	d.Phase = "apply"
	res, logs = f.do(t, r, d)
	st.Outputs = nil
	json.Unmarshal(f.states["ws1"], &st)
	if res.Status != "succeeded" || len(st.Outputs) != 0 || len(res.Resources) != 0 || strings.Contains(logs, "outputs kept") {
		t.Fatalf("destroy of the last template: %+v, outputs %v\n%s", res, st.Outputs, logs)
	}
}

// TestJobHome: Terraform gets a HOME of the job's own, so a job can't leave a
// .terraformrc or provider mirror behind for the next one.
func TestJobHome(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	j := job("home1", "plan", "apply", f.module(t, `output "home" { value = pathexpand("~") }`), nil)
	res, logs := f.do(t, r, j)
	if res.Status != "succeeded" {
		t.Fatalf("plan: %+v\n%s", res, logs)
	}
	if !regexp.MustCompile(`home\s*=\s*"[^"]*/ops-home-[^"/]*"`).MatchString(logs) {
		t.Fatalf("HOME must be the job's own:\n%s", logs)
	}
}

// The shipper masks a signed URL even when it arrives split over two writes, and its final flush
// ships a tail larger than one post.
func TestShipperRedactsAndDrains(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signed := "/v1/archives/m.tar.gz?expires=1&sig=secret-signature"
	pem := "-----BEGIN KEY-----\nAAAAkeyline\n-----END KEY-----"
	s := newShipper(r, runnerapi.Job{RunID: "run1", LeaseID: "lease", Secrets: []string{"hunter22", "hunter22-long", pem, "eu"}}, func() {}, []string{signed}, time.Now().Add(time.Hour))
	s.Write([]byte("init " + signed[:30]))
	s.Write([]byte(signed[30:] + " failed\n"))
	s.Write([]byte("local-exec: hunter22-long hunter22 eu AAAAkeyline\n"))
	big := strings.Repeat("x", 300<<10) + "\nthe error at the end"
	s.Write([]byte(big))
	s.Close()
	got := f.logs.String()
	if strings.Contains(got, "secret-signature") || !strings.Contains(got, "?<signed>") {
		t.Fatalf("signed URL not masked: %.200q", got)
	}
	if !strings.Contains(got, "local-exec: *** *** eu ***\n") {
		t.Fatalf("sensitive values not masked (whole, per line, not too short): %.400q", got)
	}
	if !strings.HasSuffix(got, big) {
		t.Fatalf("tail lost: got %d bytes, want suffix of %d", len(got), len(big))
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

// After the phase's hard end the shipper neither posts logs nor heartbeats, so the lease runs out
// unless the runner finishes; Close still ships the tail once.
func TestShipperStopsAtHardEnd(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// a heartbeat every 2s otherwise
	s := newShipper(r, runnerapi.Job{RunID: "run1", LeaseID: "lease", LeaseSeconds: 8}, func() {}, nil, time.Now())
	s.Write([]byte("after the end\n"))
	time.Sleep(3500 * time.Millisecond)
	f.mu.Lock()
	calls := slices.Clone(f.calls)
	f.mu.Unlock()
	s.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(calls) != 0 || !slices.Equal(f.calls, []string{"logs"}) || f.logs.String() != "after the end\n" {
		t.Fatalf("calls before Close %v, after %v, logs %q", calls, f.calls, f.logs.String())
	}
}

func TestEstimate(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(t.TempDir(), "infracost")
	// Fake infracost: checks it gets the plan JSON and only its own key, then answers like breakdown.
	script := `#!/bin/sh
[ "$INFRACOST_API_KEY" = key ] && [ -z "$ARM_CLIENT_SECRET" ] && grep -q format_version "$3" || exit 1
echo '{"currency":"'"${INFRACOST_CURRENCY:-USD}"'","pastTotalMonthlyCost":"10","totalMonthlyCost":"25.5","diffTotalMonthlyCost":"15.5","summary":{"totalSupportedResources":2,"totalUnsupportedResources":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_CLIENT_SECRET", "s3cret")
	r := &Runner{Runner: config.Runner{Infracost: bin, InfracostAPIKey: "key"}}
	var log strings.Builder
	c := r.estimate(context.Background(), dir, &tfjson.Plan{FormatVersion: "1.2"}, "EUR", &log)
	want := &runnerapi.Cost{Currency: "EUR", Prior: 10, Proposed: 25.5, Delta: 15.5, Priced: 2, Unsupported: 1}
	if !reflect.DeepEqual(c, want) || !strings.Contains(log.String(), "EUR 25.50 (+15.50)") {
		t.Fatalf("estimate: %+v\n%s", c, log.String())
	}

	// A failing infracost leaves the plan without an estimate; off without a key.
	r.InfracostAPIKey = "wrong"
	if c := r.estimate(context.Background(), dir, &tfjson.Plan{}, "", &log); c != nil || !strings.Contains(log.String(), "cost estimation skipped") {
		t.Fatalf("failed estimate: %+v\n%s", c, log.String())
	}
	r.InfracostAPIKey = ""
	if c := r.estimate(context.Background(), dir, &tfjson.Plan{}, "", io.Discard); c != nil {
		t.Fatalf("estimate without a key: %+v", c)
	}
}

// A claimed job can be far bigger than 64 KiB (one variable alone may be); over MaxJobSize is a clear error.
func TestClaimLargeJob(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	vars := map[string]any{}
	for i := range 16 {
		vars[strings.Repeat("v", i+1)] = strings.Repeat("x", 64<<10)
	}
	body, _ = json.Marshal(runnerapi.Job{RunID: "r1", Vars: vars})
	j, ok, err := r.claim(context.Background())
	if err != nil || !ok || j.RunID != "r1" || len(j.Vars) != 16 {
		t.Fatalf("claim of %d bytes: ok=%v err=%v", len(body), ok, err)
	}
	body = []byte(`{"vars":{"v":"` + strings.Repeat("x", runnerapi.MaxJobSize) + `"}}`)
	if _, _, err := r.claim(context.Background()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("oversized claim: err = %v", err)
	}
}

// A drain while a claim is in flight lets the claim complete: the job it returns is acknowledged
// before anything else and run to the end, then the runner stops.
func TestDrainKeepsClaimedJob(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	drain, stop := context.WithCancel(context.Background())
	defer stop()
	claims := 0
	j := job("run-drain", "plan", "apply", f.module(t, helloModule), map[string]any{"greeting": "hi"})
	f.stateToken, f.planning = j.StateToken, true
	f.claim = func(w http.ResponseWriter, _ *http.Request) {
		if claims++; claims > 1 {
			t.Error("claimed again after the drain")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		stop() // the drain arrives while the server answers this claim
		json.NewEncoder(w).Encode(j)
	}
	done := make(chan error)
	go func() { done <- r.Run(drain, context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("runner didn't stop")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 || f.calls[0] != "ack" || f.calls[len(f.calls)-1] != "finish" || f.finish == nil || f.finish.Status != "succeeded" {
		t.Fatalf("calls %v, result %+v\n%s", f.calls, f.finish, f.logs.String())
	}
}

// A claim response that doesn't decode isn't acknowledged (the server queues the job again); the
// runner goes on claiming.
func TestUnreadableClaim(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	drain, stop := context.WithCancel(context.Background())
	defer stop()
	claims := 0
	f.claim = func(w http.ResponseWriter, _ *http.Request) {
		if claims++; claims == 1 {
			w.Write([]byte(`{"run_id": "r1", "lease_id": `)) // cut off
			return
		}
		stop()
		w.WriteHeader(http.StatusNoContent)
	}
	if err := r.Run(drain, context.Background()); err != nil {
		t.Fatal(err)
	}
	if claims != 2 || len(f.calls) != 0 {
		t.Fatalf("claims %d, job calls %v: want a second claim and no ack", claims, f.calls)
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
