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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"

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
	r.Log.Info("runner started", "name", r.Name, "server", r.ServerURL.String(), "backend", r.Backend)
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

// Execute runs one job and reports its result, unless the lease was lost on the way.
func (r *Runner) Execute(hard context.Context, job runnerapi.Job) {
	log := r.Log.With("deployment", job.DeploymentID)
	log.Info("job started")
	ctx, cancel := context.WithTimeout(hard, r.JobTimeout)
	defer cancel()

	src := job.ModuleSource
	if strings.HasPrefix(src, "/") { // this server's registry: a short-lived signed archive URL
		rel, err := url.Parse(src)
		if err != nil {
			r.finish(log, job, "failed", "invalid module source from server", nil)
			return
		}
		src = r.ServerURL.ResolveReference(rel).String()
	}
	ship := newShipper(r, job, cancel, src)
	status, msg, outputs := r.terraform(ctx, job, src, ship)
	ship.Close()
	if ship.lost.Load() {
		log.Warn("lease lost; result not reported")
		return
	}
	switch {
	case hard.Err() != nil:
		status, msg = "canceled", "runner was shut down during the run"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status, msg = "failed", fmt.Sprintf("timed out after %s", r.JobTimeout)
	}
	r.finish(log, job, status, msg, outputs)
}

func (r *Runner) terraform(ctx context.Context, job runnerapi.Job, src string, w io.Writer) (status, msg string, outputs map[string]any) {
	fail := func(step string, err error) (string, string, map[string]any) {
		fmt.Fprintf(w, "\n==> %s failed\n", step)
		m := step + ": " + err.Error()
		if len(m) > 2000 {
			m = m[:2000] + "…"
		}
		return "failed", m, nil
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

	fmt.Fprintf(w, "==> runner %s: deployment %s\n==> terraform init (fetch module)\n", r.Name, job.DeploymentID)
	if err := tf.Init(ctx, tfexec.FromModule(src), tfexec.Backend(false)); err != nil {
		return fail("fetch module", err)
	}
	backend := fmt.Sprintf("terraform {\n  backend %q {}\n}\n", r.Backend)
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
	opts, err := r.backendOptions(job.StateKey)
	if err != nil {
		return fail("configure backend", err)
	}

	fmt.Fprintf(w, "\n==> terraform init (%s backend)\n", r.Backend)
	if err := tf.Init(ctx, append(opts, tfexec.Reconfigure(true))...); err != nil {
		return fail("init", err)
	}
	destroy := job.Action == "destroy"
	step := "plan"
	if destroy {
		step = "plan -destroy"
	}
	fmt.Fprintf(w, "\n==> terraform %s\n", step)
	changes, err := tf.Plan(ctx, tfexec.Out("form2stack.tfplan"), tfexec.Destroy(destroy))
	if err != nil {
		return fail("plan", err)
	}
	if changes {
		fmt.Fprintln(w, "\n==> terraform apply")
		if err := tf.Apply(ctx, tfexec.DirOrPlan("form2stack.tfplan")); err != nil {
			return fail("apply", err)
		}
	} else {
		fmt.Fprintln(w, "\n==> no changes, skipping apply")
	}
	if destroy {
		fmt.Fprintln(w, "\n==> done")
		return "succeeded", "", nil
	}
	// `output -json` prints sensitive values in clear text: keep it out of the streamed log.
	tf.SetStdout(io.Discard)
	tf.SetStderr(io.Discard)
	outs, err := tf.Output(ctx)
	if err != nil {
		return fail("read outputs", err)
	}
	outputs = map[string]any{}
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
	return "succeeded", "", outputs
}

// backendOptions points the configured backend at this deployment's own state.
// shortcut: key-based backends (azurerm, s3, ...) plus local; gcs ("prefix") etc. need a case here.
func (r *Runner) backendOptions(stateKey string) ([]tfexec.InitOption, error) {
	var opts []tfexec.InitOption
	for _, kv := range r.BackendConfig {
		opts = append(opts, tfexec.BackendConfig(kv))
	}
	if r.Backend != "local" {
		return append(opts, tfexec.BackendConfig("key="+stateKey)), nil
	}
	if !filepath.IsLocal(stateKey) {
		return nil, fmt.Errorf("unsafe state key %q", stateKey)
	}
	path := filepath.Join(r.StateDir, stateKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return append(opts, tfexec.BackendConfig("path="+path)), nil
}

func (r *Runner) finish(log *slog.Logger, job runnerapi.Job, status, msg string, outputs map[string]any) {
	body, _ := json.Marshal(runnerapi.FinishRequest{Status: status, Message: msg, Outputs: outputs})
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		code, resp, err := r.call(ctx, "/jobs/"+job.DeploymentID+"/finish", job.LeaseID, "application/json", body)
		cancel()
		if err == nil && code == http.StatusNoContent {
			log.Info("job finished", "status", status)
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

func (r *Runner) call(ctx context.Context, path, lease, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ServerURL.JoinPath(runnerapi.Prefix, path).String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set("Content-Type", contentType)
	if lease != "" {
		req.Header.Set(runnerapi.LeaseHeader, lease)
	}
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
	r       *Runner
	job     runnerapi.Job
	cancel  context.CancelFunc
	redact  *strings.Replacer
	mu      sync.Mutex
	buf     bytes.Buffer
	lost    atomic.Bool
	stop    chan struct{}
	stopped chan struct{}
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
	code, _, err := s.r.call(ctx, "/jobs/"+s.job.DeploymentID+path, s.job.LeaseID, "text/plain; charset=utf-8", body)
	if err == nil && code == http.StatusConflict {
		s.lost.Store(true)
		s.cancel() // someone else owns this deployment now; stop terraform
		return false
	}
	return err == nil && code == http.StatusNoContent
}

// Close stops the loop after a final flush.
func (s *shipper) Close() {
	close(s.stop)
	<-s.stopped
}
