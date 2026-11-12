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
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

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
	if len(res.Resources) != 0 || f.states["ws1"] != nil || len(f.artifacts["run1/artifacts/tfplan"]) == 0 {
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
	if len(res.Resources) != 1 || res.Resources[0].ID == "" {
		t.Fatalf("resources without the ID: %+v", res.Resources)
	}
	if res.Resources[0].ID = ""; res.Resources[0] != wantRes {
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

// TestErroredState: state an apply couldn't save (errored.tfstate) is pushed again, or else kept
// outside the workdir, which is deleted.
func TestErroredState(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // where a kept state goes
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	src := f.module(t, helloModule)
	vars := map[string]any{"greeting": "hello"}

	f.do(t, r, job("once", "plan", "apply", src, vars))
	f.failWrites = 1 // the apply's write fails, the push's succeeds
	res, logs := f.do(t, r, job("once", "apply", "apply", src, vars))
	if res.Status != "failed" || !strings.HasPrefix(res.Message, "the state was saved on a second try; apply: ") ||
		!strings.Contains(string(f.states["ws1"]), `"terraform_data"`) || len(res.Resources) != 1 {
		t.Fatalf("pushed: %+v %q\n%s", res, f.states["ws1"], logs)
	}

	f.do(t, r, job("twice", "plan", "apply", src, map[string]any{"greeting": "hi"}))
	f.failWrites = 2 // the push fails too
	res, logs = f.do(t, r, job("twice", "apply", "apply", src, map[string]any{"greeting": "hi"}))
	kept := filepath.Join(os.TempDir(), "opsolete-errored-twice.tfstate.sealed")
	b, err := os.ReadFile(kept)
	if res.Status != "failed" || !strings.Contains(res.Message, kept) || err != nil || strings.Contains(string(b), `"hi"`) {
		t.Fatalf("kept sealed: %+v %v\n%s", res, err, logs)
	}
	if fi, _ := os.Stat(kept); fi.Mode().Perm() != 0o600 {
		t.Errorf("kept state mode %v", fi.Mode().Perm())
	}
	if plain, err := UnsealState(b, r.Token); err != nil || !strings.Contains(plain, `"hi"`) {
		t.Errorf("unseal with the runner token: %v", err)
	}
	if _, err := UnsealState(b, "another-runner-token-another-runner-token"); err == nil {
		t.Error("unsealed with another token")
	}
}

// TestJobEnv: a job's environment variables reach Terraform, on top of the runner's own.
func TestJobEnv(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := testRunner(t, srv)
	j := job("env1", "plan", "apply", f.module(t, helloModule), map[string]any{"greeting": "hi"})
	// not under /tmp: the job sandbox hides the runner's /tmp, but for the job's own directories
	parent, err := os.MkdirTemp(".", ".test-tfdata-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	data, _ := filepath.Abs(filepath.Join(parent, "tfdata"))
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

// TestUnknownAction: a job of an action this runner doesn't know (a newer server) fails, saying why.
func TestUnknownAction(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	res, _ := f.do(t, testRunner(t, srv), job("u1", "plan", "teleport", "", nil))
	if res.Status != "failed" || !strings.Contains(res.Message, `doesn't know the action "teleport"`) {
		t.Fatalf("%+v", res)
	}
}

// TestOnce: a one-shot runner (a pod per phase) exits after one job, or after one claim that finds none (another
// runner took it); a claim that fails is retried.
func TestOnce(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok", Name: "test", JobTimeout: time.Minute, JobGrace: time.Minute, Once: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	claims := 0
	f.claim = func(w http.ResponseWriter, _ *http.Request) {
		if claims++; claims == 1 {
			w.Write([]byte(`{"run_id": "r1", "lease_id": `)) // cut off: not acknowledged, claimed again
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	if err := r.Run(context.Background(), context.Background()); err != nil || claims != 2 {
		t.Fatalf("no job: %v, claims %d", err, claims)
	}
	claims = 0
	f.claim = func(w http.ResponseWriter, _ *http.Request) {
		claims++
		json.NewEncoder(w).Encode(job("o1", "plan", "teleport", "", nil))
	}
	if err := r.Run(context.Background(), context.Background()); err != nil || claims != 1 || f.finish == nil || f.finish.Status != "failed" {
		t.Fatalf("one job: %v, claims %d, result %+v", err, claims, f.finish)
	}
}
