// Package runner executes Form2Stack deployments with Terraform. It pulls jobs from the
// server's runner API, runs init/plan/apply (or a destroy plan) via terraform-exec in a throwaway directory,
// ships output back as it happens and reports the result with the terraform outputs.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/rudesome/form2stack/internal/config"
	"github.com/rudesome/form2stack/internal/runnerapi"
)

var errUnauthorized = errors.New("server rejected the runner token")

type Runner struct {
	config.Runner
	HTTP *http.Client
	Log  *slog.Logger
}

func New(c config.Runner, log *slog.Logger) *Runner {
	return &Runner{Runner: c, HTTP: &http.Client{}, Log: log}
}

// Run claims and executes jobs one at a time until ctx is cancelled. Cancelling ctx stops
// claiming but lets the current job finish; cancelling hard interrupts terraform (SIGINT,
// so it can release its state lock).
func (r *Runner) Run(ctx, hard context.Context) error {
	r.Log.Info("runner started", "name", r.Name, "server", r.ServerURL.String())
	backoff := time.Second
	for ctx.Err() == nil {
		job, ok, err := r.claim(ctx)
		if errors.Is(err, errUnauthorized) {
			return err
		}
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.Log.Warn("claim failed, retrying", "err", err, "in", backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if ok {
			r.Execute(hard, job)
		}
	}
	r.Log.Info("runner stopped")
	return nil
}

func (r *Runner) claim(ctx context.Context) (runnerapi.Job, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second) // server long-polls ~20s
	defer cancel()
	body, _ := json.Marshal(runnerapi.ClaimRequest{Runner: r.Name})
	code, resp, err := r.call(ctx, "/jobs/claim", "", "application/json", body)
	if err != nil {
		return runnerapi.Job{}, false, err
	}
	switch code {
	case http.StatusOK:
		var j runnerapi.Job
		return j, true, json.Unmarshal(resp, &j)
	case http.StatusNoContent:
		return runnerapi.Job{}, false, nil
	case http.StatusUnauthorized:
		return runnerapi.Job{}, false, errUnauthorized
	default:
		return runnerapi.Job{}, false, fmt.Errorf("claim: HTTP %d: %s", code, resp)
	}
}

// Execute runs one phase of a run and reports its result, unless the lease was lost on the way.
func (r *Runner) Execute(hard context.Context, job runnerapi.Job) {
	log := r.Log.With("run", job.RunID, "phase", job.Phase)
	log.Info("job started")
	ctx, cancel := context.WithTimeout(hard, r.JobTimeout)
	defer cancel()

	src := job.ModuleSource
	if strings.HasPrefix(src, "/") { // this server's registry: a short-lived signed archive URL
		rel, err := url.Parse(src)
		if err != nil {
			r.finish(log, job, runnerapi.FinishRequest{Status: "failed", Message: "invalid module source from server"})
			return
		}
		src = r.ServerURL.ResolveReference(rel).String()
	}
	ship := newShipper(r, job, cancel, src)
	res := r.terraform(ctx, job, src, ship)
	ship.Close()
	if ship.lost.Load() {
		log.Warn("lease lost; result not reported")
		return
	}
	switch {
	case hard.Err() != nil:
		res.Status, res.Message = "canceled", "runner was shut down during the run"
	case ship.canceled.Load():
		res.Status, res.Message = "canceled", "canceled by a user; Terraform was interrupted and saved its state"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status, res.Message = "failed", fmt.Sprintf("timed out after %s", r.JobTimeout)
	}
	r.finish(log, job, res)
}

const (
	planFile = "form2stack.tfplan"
	lockFile = ".terraform.lock.hcl"
)

// terraform runs one phase in a throwaway directory. plan: save a plan, summarize it and upload
// it with the provider lock file. apply: restore both, then apply exactly that plan.
func (r *Runner) terraform(ctx context.Context, job runnerapi.Job, src string, w io.Writer) (res runnerapi.FinishRequest) {
	fail := func(step string, err error) runnerapi.FinishRequest {
		fmt.Fprintf(w, "\n==> %s failed\n", step)
		m := step + ": " + err.Error()
		if len(m) > 2000 {
			m = m[:2000] + "…"
		}
		return runnerapi.FinishRequest{Status: "failed", Message: m}
	}
	dir, err := os.MkdirTemp("", "f2s-job-*")
	if err != nil {
		return fail("prepare workdir", err)
	}
	defer os.RemoveAll(dir)
	tf, err := tfexec.NewTerraform(dir, r.Terraform)
	if err != nil {
		return fail("prepare terraform", err)
	}
	tf.SetStdout(w)
	tf.SetStderr(w)
	if len(job.Env) > 0 {
		env := map[string]string{}
		for _, kv := range os.Environ() {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		maps.Copy(env, job.Env)
		// CleanEnv drops the few variables terraform-exec sets itself (TF_LOG, TF_INPUT, ...).
		if err := tf.SetEnv(tfexec.CleanEnv(env)); err != nil {
			return fail("set environment", err)
		}
	}

	fmt.Fprintf(w, "==> runner %s: %s phase of %s run %s\n==> terraform init (fetch module)\n", r.Name, job.Phase, job.Action, job.RunID)
	if err := tf.Init(ctx, tfexec.FromModule(src), tfexec.Backend(false)); err != nil {
		return fail("fetch module", err)
	}
	backend := "terraform {\n  backend \"http\" {}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "form2stack_backend.tf"), []byte(backend), 0o600); err != nil {
		return fail("write backend", err)
	}
	if job.Vars == nil {
		job.Vars = map[string]any{} // "null" is not a valid tfvars file
	}
	vars, err := json.Marshal(job.Vars)
	if err != nil {
		return fail("encode variables", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "form2stack.auto.tfvars.json"), vars, 0o600); err != nil {
		return fail("write variables", err)
	}
	if job.Phase == "apply" {
		// Before init, so it installs exactly the providers the plan was made with.
		if err := r.download(ctx, job, runnerapi.ArtifactLockfile, filepath.Join(dir, lockFile), false); err != nil {
			return fail("download lock file", err)
		}
		if err := r.download(ctx, job, runnerapi.ArtifactPlan, filepath.Join(dir, planFile), true); err != nil {
			return fail("download plan", err)
		}
	}
	fmt.Fprintln(w, "\n==> terraform init (Form2Stack state)")
	if err := tf.Init(ctx, append(r.backendOptions(job), tfexec.Reconfigure(true))...); err != nil {
		return fail("init", err)
	}
	if job.Phase == "plan" {
		return r.plan(ctx, tf, dir, job, w, fail)
	}

	// The apply writes state: report what's in it however it ends, since a failed apply can
	// leave resources behind too.
	defer func() { res.Resources = inventory(ctx, tf) }()
	fmt.Fprintln(w, "\n==> terraform apply (saved plan)")
	if err := tf.Apply(ctx, tfexec.DirOrPlan(planFile)); err != nil {
		return fail("apply", err)
	}
	if job.Action == "destroy" {
		fmt.Fprintln(w, "\n==> done")
		return runnerapi.FinishRequest{Status: "succeeded"}
	}
	// `output -json` prints sensitive values in clear text: keep it out of the streamed log.
	tf.SetStdout(io.Discard)
	tf.SetStderr(io.Discard)
	outs, err := tf.Output(ctx)
	if err != nil {
		return fail("read outputs", err)
	}
	outputs := map[string]any{}
	for k, o := range outs {
		if o.Sensitive { // never leaves the runner
			outputs[k] = "(sensitive)"
			continue
		}
		var v any
		if err := json.Unmarshal(o.Value, &v); err == nil {
			outputs[k] = v
		}
	}
	fmt.Fprintln(w, "\n==> done")
	return runnerapi.FinishRequest{Status: "succeeded", Outputs: outputs}
}

// plan saves a plan; with changes it reports a summary and uploads the plan and the lock file
// for the apply phase.
func (r *Runner) plan(ctx context.Context, tf *tfexec.Terraform, dir string, job runnerapi.Job, w io.Writer,
	fail func(string, error) runnerapi.FinishRequest) runnerapi.FinishRequest {
	destroy := job.Action == "destroy"
	step := "plan"
	if destroy {
		step = "plan -destroy"
	}
	fmt.Fprintf(w, "\n==> terraform %s\n", step)
	changes, err := tf.Plan(ctx, tfexec.Out(planFile), tfexec.Destroy(destroy))
	if err != nil {
		return fail("plan", err)
	}
	if !changes {
		fmt.Fprintln(w, "\n==> no changes")
		return runnerapi.FinishRequest{Status: "succeeded"}
	}
	// `show -json` of a plan includes variable and attribute values: keep it out of the log.
	tf.SetStdout(io.Discard)
	tf.SetStderr(io.Discard)
	p, err := tf.ShowPlanFile(ctx, planFile)
	if err != nil {
		return fail("read plan", err)
	}
	sum := summarize(p)
	for _, a := range []struct{ name, file string }{{runnerapi.ArtifactPlan, planFile}, {runnerapi.ArtifactLockfile, lockFile}} {
		b, err := os.ReadFile(filepath.Join(dir, a.file))
		if errors.Is(err, os.ErrNotExist) && a.file == lockFile {
			continue // no external providers, so no lock file
		}
		if err == nil {
			err = r.upload(ctx, job, a.name, b)
		}
		if err != nil {
			return fail("save plan", err)
		}
	}
	fmt.Fprintf(w, "\n==> plan saved: %d to add, %d to change, %d to destroy\n", sum.Add, sum.Change, sum.Destroy)
	return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum}
}

// summarize counts a plan's resource changes, like Terraform's "Plan: x to add" line, and lists
// their addresses. Values are never included.
func summarize(p *tfjson.Plan) runnerapi.Plan {
	sum := runnerapi.Plan{Changes: []runnerapi.Change{}}
	for _, rc := range p.ResourceChanges {
		if rc.Change == nil {
			continue
		}
		a := rc.Change.Actions
		var action string
		switch {
		case a.Replace():
			sum.Add++
			sum.Destroy++
			action = "replace"
		case a.Create():
			sum.Add++
			action = "create"
		case a.Update():
			sum.Change++
			action = "update"
		case a.Delete():
			sum.Destroy++
			action = "delete"
		default: // no-op, read, forget
			continue
		}
		sum.Changes = append(sum.Changes, runnerapi.Change{Address: rc.Address, Action: action})
	}
	return sum
}

// inventory lists the managed resources in the workspace's state: names only, never attribute
// values. nil means the state couldn't be read, so the current state version stays unindexed.
func inventory(ctx context.Context, tf *tfexec.Terraform) []runnerapi.Resource {
	// Own context: after a cancel the job's context is done, but while the run holds its lease
	// the state is still readable, and a half-applied state is the one most worth indexing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	tf.SetStdout(io.Discard) // show -json prints the whole state, secrets included
	tf.SetStderr(io.Discard)
	st, err := tf.Show(ctx)
	if err != nil {
		return nil
	}
	out := []runnerapi.Resource{}
	var walk func(m *tfjson.StateModule)
	walk = func(m *tfjson.StateModule) {
		for _, r := range m.Resources {
			if r.Mode == tfjson.ManagedResourceMode {
				out = append(out, runnerapi.Resource{Address: r.Address, Type: r.Type, Name: r.Name, Provider: r.ProviderName, Module: m.Address})
			}
		}
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	if st.Values != nil && st.Values.RootModule != nil {
		walk(st.Values.RootModule)
	}
	return out
}

// backendOptions points Terraform's http backend at the workspace's state on the server. The
// password only works while this run is planning or applying. Terraform keeps it in .terraform/ and
// in the saved plan (module code could read it during the run): it only reaches this workspace's state.
func (r *Runner) backendOptions(job runnerapi.Job) []tfexec.InitOption {
	addr := r.ServerURL.JoinPath(runnerapi.StatePrefix, job.WorkspaceID).String()
	return []tfexec.InitOption{
		tfexec.BackendConfig("address=" + addr),
		tfexec.BackendConfig("lock_address=" + addr),
		tfexec.BackendConfig("unlock_address=" + addr),
		tfexec.BackendConfig("username=" + job.WorkspaceID),
		tfexec.BackendConfig("password=" + job.StateToken),
	}
}

func (r *Runner) finish(log *slog.Logger, job runnerapi.Job, res runnerapi.FinishRequest) {
	body, _ := json.Marshal(res)
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		code, resp, err := r.call(ctx, "/jobs/"+job.RunID+"/finish", job.LeaseID, "application/json", body)
		cancel()
		if err == nil && code == http.StatusNoContent {
			log.Info("job finished", "status", res.Status)
			return
		}
		if err == nil && code < 500 {
			log.Error("finish rejected", "code", code, "body", string(resp))
			return
		}
		if attempt == 5 {
			// The server fails the job itself once the lease expires.
			log.Error("could not report result", "err", err, "code", code)
			return
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
}

// upload stores a plan phase's artifact on the server.
func (r *Runner) upload(ctx context.Context, job runnerapi.Job, name string, b []byte) error {
	code, resp, err := r.call(ctx, "/jobs/"+job.RunID+"/artifacts/"+name, job.LeaseID, "application/octet-stream", b)
	if err == nil && code != http.StatusNoContent {
		err = fmt.Errorf("HTTP %d: %s", code, resp)
	}
	return err
}

// download writes an artifact of the run's plan phase to path; a missing optional one is skipped.
func (r *Runner) download(ctx context.Context, job runnerapi.Job, name, path string, required bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ServerURL.JoinPath(runnerapi.Prefix, "jobs", job.RunID, "artifacts", name).String(), nil)
	if err != nil {
		return err
	}
	r.authorize(req, job.LeaseID)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && !required {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (r *Runner) authorize(req *http.Request, lease string) {
	req.Header.Set("Authorization", "Bearer "+r.Token)
	if lease != "" {
		req.Header.Set(runnerapi.LeaseHeader, lease)
	}
}

func (r *Runner) call(ctx context.Context, path, lease, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ServerURL.JoinPath(runnerapi.Prefix, path).String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	r.authorize(req, lease)
	req.Header.Set("Content-Type", contentType)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// shipper buffers job output and posts it to the server about once a second, cut at line
// boundaries. Posts double as heartbeats; a 409 means the lease is gone and the job is cancelled.
type shipper struct {
	r        *Runner
	job      runnerapi.Job
	cancel   context.CancelFunc
	redact   *strings.Replacer
	mu       sync.Mutex
	buf      bytes.Buffer
	lost     atomic.Bool
	canceled atomic.Bool // a user canceled the run: Terraform is interrupted, the lease kept
	stop     chan struct{}
	stopped  chan struct{}
}

const maxBuffered = 8 << 20 // if the server is unreachable for long, drop output rather than memory

func newShipper(r *Runner, job runnerapi.Job, cancel context.CancelFunc, src string) *shipper {
	redacted := src
	if i := strings.Index(src, "?"); i >= 0 && strings.HasPrefix(job.ModuleSource, "/") {
		redacted = src[:i] + "?<signed>"
	}
	s := &shipper{r: r, job: job, cancel: cancel, redact: strings.NewReplacer(src, redacted),
		stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

func (s *shipper) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() < maxBuffered {
		s.buf.WriteString(s.redact.Replace(string(p)))
	}
	return len(p), nil
}

func (s *shipper) loop() {
	defer close(s.stopped)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	beat := time.Duration(max(s.job.LeaseSeconds, 8)) * time.Second / 4
	last := time.Now()
	for {
		select {
		case <-s.stop:
			s.flush(true)
			return
		case <-tick.C:
			if s.flush(false) {
				last = time.Now()
			} else if time.Since(last) > beat {
				s.post("/heartbeat", nil)
				last = time.Now()
			}
		}
	}
}

// flush sends buffered output; unless final, only up to the last newline. Reports whether it sent.
func (s *shipper) flush(final bool) bool {
	s.mu.Lock()
	data := s.buf.Bytes()
	n := len(data)
	if !final {
		n = bytes.LastIndexByte(data, '\n') + 1
	}
	n = min(n, 256<<10)
	chunk := bytes.Clone(data[:n])
	s.buf.Next(n)
	s.mu.Unlock()
	if len(chunk) == 0 {
		return false
	}
	if !s.post("/logs", chunk) && !s.lost.Load() {
		s.mu.Lock() // keep it for the next attempt
		rest := bytes.Clone(s.buf.Bytes())
		s.buf.Reset()
		s.buf.Write(chunk)
		s.buf.Write(rest)
		s.mu.Unlock()
	}
	return true
}

func (s *shipper) post(path string, body []byte) bool {
	if s.lost.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, _, err := s.r.call(ctx, "/jobs/"+s.job.RunID+path, s.job.LeaseID, "text/plain; charset=utf-8", body)
	if err == nil && code == http.StatusConflict {
		s.lost.Store(true)
		s.cancel() // someone else owns this run now; stop terraform
		return false
	}
	if err == nil && code == runnerapi.StatusCancel {
		if !s.canceled.Swap(true) {
			s.cancel() // SIGINT: Terraform finishes in-flight operations and saves state
		}
		return true
	}
	return err == nil && code == http.StatusNoContent
}

// Close stops the loop after a final flush.
func (s *shipper) Close() {
	close(s.stop)
	<-s.stopped
}
