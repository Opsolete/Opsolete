// Package runner executes Opsolete deployments with Terraform. It pulls jobs from the
// server's runner API, runs init/plan/apply (or a destroy plan) in a throwaway directory,
// ships output back as it happens and reports the result with the terraform outputs.
package runner

import (
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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/secrets"
)

type Runner struct {
	config.Runner
	HTTP *http.Client
	Log  *slog.Logger
	self string // the runner's binary, which starts the job sandbox's helper; "" no sandbox (setupSandbox)
}

func New(c config.Runner, log *slog.Logger) *Runner {
	return &Runner{Runner: c, HTTP: &http.Client{}, Log: log}
}

// Run claims and executes jobs one at a time until ctx is cancelled. Cancelling ctx stops
// claiming but lets a claim in flight and the current job finish (a claim the server committed
// is a job: dropping it would strand it); cancelling hard interrupts terraform (SIGINT, so it can
// release its state lock) and a claim in flight.
func (r *Runner) Run(ctx, hard context.Context) error {
	if err := hardenProcess(); err != nil {
		return fmt.Errorf("harden runner process: %w", err)
	}
	if err := r.setupSandbox(); err != nil {
		return err
	}
	r.Log.Info("runner started", "name", r.Name, "server", r.ServerURL.String())
	if !isolates {
		r.Log.Warn("not on Linux: processes a job leaves behind are not killed, so jobs aren't isolated from each other")
	}
	backoff := time.Second
	for ctx.Err() == nil {
		job, ok, err := r.claim(hard)
		if errors.Is(err, errUnauthorized) {
			return err
		}
		if err != nil {
			if hard.Err() != nil {
				break
			}
			logf := r.Log.Warn
			if errors.Is(err, errUnreadableJob) {
				logf = r.Log.Error
			}
			logf("claim failed, retrying", "err", err, "in", backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if ok && r.ack(hard, job) {
			r.Execute(hard, job)
		}
		if r.Once { // one job, or none within one claim: another runner took it
			break
		}
	}
	r.Log.Info("runner stopped")
	return nil
}

// Execute runs one phase of a run and reports its result, unless the lease was lost on the way.
func (r *Runner) Execute(hard context.Context, job runnerapi.Job) {
	log := r.Log.With("run", job.RunID, "phase", job.Phase)
	log.Info("job started")
	// One deadline per phase; Terraform gets JobGrace after it (or any earlier interrupt), never more.
	deadline := time.Now().Add(r.JobTimeout)
	ctx, cancel := context.WithDeadline(hard, deadline)
	defer cancel()
	p := &tfProc{bin: r.Terraform, grace: r.JobGrace, end: deadline.Add(r.JobGrace), self: r.self}

	var signed []string // resolved signed URLs, kept out of the log
	resolve := func(src string) (string, error) {
		if !strings.HasPrefix(src, "/") {
			return src, nil
		}
		// This server's registry: a short-lived signed archive URL.
		rel, err := url.Parse(src)
		if err != nil {
			return "", err
		}
		abs := r.ServerURL.ResolveReference(rel).String()
		signed = append(signed, abs)
		return abs, nil
	}
	src, err := resolve(job.ModuleSource)
	job.Modules = slices.Clone(job.Modules)
	for i := range job.Modules {
		if err == nil {
			job.Modules[i].Source, err = resolve(job.Modules[i].Source)
		}
	}
	if err != nil {
		r.finish(log, job, runnerapi.FinishRequest{Status: "failed", Message: "invalid module source from server"})
		return
	}
	ship := newShipper(r, job, cancel, signed, p.end)
	res := r.terraform(ctx, job, src, ship, p)
	if err := killJobProcesses(); err != nil {
		log.Error("processes the job left behind could not be killed: the next job may not be isolated from them", "err", err)
	}
	ship.Close()
	if ship.lost.Load() {
		log.Warn("lease lost; result not reported")
		return
	}
	stopped := "no Terraform command was running at the time" // e.g. during a plan download
	if p.interrupted.Load() {
		stopped = "Terraform was interrupted and saved its state"
	}
	if p.killed.Load() {
		stopped = fmt.Sprintf("Terraform was interrupted but didn't stop within OPS_JOB_GRACE (%s) and was killed: the state may be missing resources it was changing", r.JobGrace)
	}
	switch {
	case res.Status == "succeeded": // finished before the cancel or shutdown took effect: keep the result
	case hard.Err() != nil:
		res.Status, res.Message = "canceled", "runner was shut down during the run; "+stopped
	case ship.canceled.Load():
		res.Status, res.Message = "canceled", "canceled by a user; "+stopped
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status, res.Message = "failed", fmt.Sprintf("timed out after %s; %s", r.JobTimeout, stopped)
	}
	if p.rescued != "" { // first: the message is cut at 2000 runes
		res.Message = strings.TrimSuffix(p.rescued+"; "+res.Message, "; ")
	}
	res.Message = ship.redact.Replace(res.Message) // Terraform's stderr may quote a signed URL
	if m := []rune(res.Message); len(m) > 2000 {   // after redacting: a cut must not split a signed URL
		res.Message = string(m[:2000]) + "…"
	}
	r.finish(log, job, res)
}

// terraform runs one phase in a throwaway directory. plan: save a plan, summarize it and upload
// it with the provider lock file. apply: restore both, then apply exactly that plan.
func (r *Runner) terraform(ctx context.Context, job runnerapi.Job, src string, w io.Writer, tf *tfProc) (res runnerapi.FinishRequest) {
	fail := func(step string, err error) runnerapi.FinishRequest {
		fmt.Fprintf(w, "\n==> %s failed\n", step)
		return runnerapi.FinishRequest{Status: "failed", Message: step + ": " + err.Error()}
	}
	dir, err := os.MkdirTemp("", "ops-job-*")
	if err != nil {
		return fail("prepare workdir", err)
	}
	defer os.RemoveAll(dir)
	tf.dir = dir
	// A throwaway HOME (outside dir: init -from-module needs it empty): a job must not leave a
	// .terraformrc, .gitconfig or provider mirror (~/.terraform.d/plugins) behind for the next job.
	home, err := os.MkdirTemp("", "ops-home-*")
	if err != nil {
		return fail("prepare workdir", err)
	}
	defer os.RemoveAll(home)
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	maps.Copy(env, job.Env)
	// the http backend's defaults: a terraform_remote_state "http" data source needs only the address
	// of a workspace that shares its state with this one
	env["TF_HTTP_USERNAME"], env["TF_HTTP_PASSWORD"] = job.WorkspaceID, job.StateToken
	env["HOME"] = home
	delete(env, "XDG_DATA_HOME") // Terraform's other implied provider mirror; defaults to under HOME
	// the job's own temporary files, removed with HOME (in the sandbox /tmp is private anyway)
	tmp := filepath.Join(home, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fail("prepare workdir", err)
	}
	env["TMPDIR"], env["TMP"], env["TEMP"] = tmp, tmp, tmp
	tf.binds = []string{dir, home}
	if c := env["TF_PLUGIN_CACHE_DIR"]; c != "" { // shared on purpose; kept reachable when it's under /tmp
		tf.binds = append(tf.binds, c)
	}
	if job.Registry != nil { // modules from the server's registry, e.g. ones a template's module calls
		modules := strings.TrimSuffix(r.ServerURL.String(), "/") + "/v1/modules/"
		if env["TF_CLI_CONFIG_FILE"], err = writeCLIConfig(home, *job.Registry, modules, env["TF_CLI_CONFIG_FILE"]); err != nil {
			return fail("prepare workdir", err)
		}
	}
	// CleanEnv drops the variables terraform-exec manages (TF_LOG, TF_INPUT, TF_CLI_ARGS, ...).
	tf.env = []string{"TF_IN_AUTOMATION=1"} // what terraform-exec adds
	for k, v := range tfexec.CleanEnv(env) {
		tf.env = append(tf.env, k+"="+v)
	}
	debug, _ := strconv.ParseBool(job.Env["OPS_DEBUG"]) // a workspace env variable: full warnings
	run := func(args ...string) (bool, error) {
		if !debug && (args[0] == "plan" || args[0] == "apply") {
			// one line per warning, e.g. the -target warning of every template removal
			args = slices.Insert(args, 1, "-compact-warnings")
		}
		return tf.run(ctx, w, w, args...)
	}

	fmt.Fprintf(w, "==> runner %s: %s phase of %s run %s\n", r.Name, job.Phase, job.Action, job.RunID)
	if !slices.Contains([]string{"apply", "destroy", "refresh", "assess", "plan"}, job.Action) {
		// a server newer than this runner: runners and servers run the same version
		return fail("job", fmt.Errorf("this runner doesn't know the action %q: upgrade it to the server's version", job.Action))
	}
	if src != "" {
		fmt.Fprintln(w, "==> terraform init (fetch module)")
		if _, err := run("init", "-input=false", "-from-module="+src, "-backend=false"); err != nil {
			return fail("fetch module", err)
		}
	}
	if len(job.Modules) > 0 {
		if err := writeModules(dir, job.Modules); err != nil {
			return fail("write templates", err)
		}
	}
	backend := "terraform {\n  backend \"http\" {}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "opsolete_backend.tf"), []byte(backend), 0o600); err != nil {
		return fail("write backend", err)
	}
	if job.Vars == nil {
		job.Vars = map[string]any{} // "null" is not a valid tfvars file
	}
	vars, err := json.Marshal(job.Vars)
	if err != nil {
		return fail("encode variables", err)
	}
	if err := os.WriteFile(filepath.Join(dir, varsFile), vars, 0o600); err != nil {
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
	fmt.Fprintln(w, "\n==> terraform init (Opsolete state)")
	if _, err := run(append([]string{"init", "-input=false", "-reconfigure"}, r.backendArgs(job)...)...); err != nil {
		return fail("init", err)
	}
	if len(job.Modules) > 0 { // after init: it reads the installed modules
		if err := writeOutputs(dir, job.Modules); err != nil {
			return fail("write template outputs", err)
		}
	}
	if job.Phase == "plan" {
		res := r.plan(ctx, tf, run, dir, job, w, fail)
		if res.Status == "succeeded" && job.Action != "plan" && job.Action != "assess" {
			// Index the state as it is (the run holds its lock): the import guard (storage.importConflict)
			// checks the resources it holds without waiting for an apply.
			res.Resources = inventory(ctx, tf)
		}
		return res
	}

	// The apply writes state: report what's in it however it ends, since a failed apply can
	// leave resources behind too.
	defer func() { res.Resources = inventory(ctx, tf) }()
	defer func() { tf.rescued = r.rescueState(ctx, tf, job, w) }() // before inventory (LIFO) and before dir is removed
	fmt.Fprintln(w, "\n==> terraform apply (saved plan)")
	if _, err := run("apply", "-auto-approve", "-input=false", planFile); err != nil {
		return fail("apply", err)
	}
	if job.Action == "destroy" {
		if len(job.Target) > 0 {
			if err := dropRemoved(ctx, tf, run, dir, job, w); err != nil {
				// the destroy itself succeeded: the next apply drops them instead
				fmt.Fprintf(w, "\n==> outputs kept until the next apply: %v\n", err)
			}
		}
		fmt.Fprintln(w, "\n==> done")
		return runnerapi.FinishRequest{Status: "succeeded"}
	}
	var outs map[string]struct {
		Sensitive bool
		Value     json.RawMessage
	}
	if err := tf.json(ctx, &outs, "output", "-no-color", "-json"); err != nil { // sensitive values in clear text
		return fail("read outputs", err)
	}
	outputs := map[string]any{}
	secrets := mask.New(job.Secrets) // an output echoing a sensitive variable or input never leaves the runner either
	for k, o := range outs {
		if o.Sensitive { // never leaves the runner
			outputs[k] = "(sensitive)"
			continue
		}
		var v any
		if err := json.Unmarshal(o.Value, &v); err == nil {
			outputs[k] = secrets.Value(v)
		}
	}
	fmt.Fprintln(w, "\n==> done")
	return runnerapi.FinishRequest{Status: "succeeded", Outputs: outputs}
}

// erroredState is where Terraform writes the state it couldn't save to the backend (in its working dir).
const erroredState = "errored.tfstate"

// rescueState handles the state an apply couldn't save: it tracks resources the apply changed, so
// losing it with the workdir orphans them. It pushes it again; if that fails too (lease lost,
// server unreachable), it moves it out of the workdir and says where. "" if there was none.
func (r *Runner) rescueState(ctx context.Context, tf *tfProc, job runnerapi.Job, w io.Writer) string {
	file := filepath.Join(tf.dir, erroredState)
	if _, err := os.Stat(file); err != nil {
		return ""
	}
	// own context, like inventory: the job's may be done (cancel, timeout), the phase's hard end isn't
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), min(time.Minute, time.Until(tf.end)))
	defer cancel()
	fmt.Fprintln(w, "\n==> terraform state push (the state the apply couldn't save)")
	if _, err := tf.run(pctx, w, w, "state", "push", "-no-color", erroredState); err == nil {
		fmt.Fprintln(w, "\n==> state saved")
		return "the state was saved on a second try"
	}
	// Jobs run as the runner's user, so the next job (any workspace's) could read a plain file: it's
	// sealed with the runner token, which jobs can't read (unset, process not dumpable).
	// shortcut: kept in the runner's temp dir, which an ephemeral container loses on restart; upload it
	// to the server instead if that matters.
	kept := filepath.Join(os.TempDir(), "opsolete-errored-"+job.RunID+".tfstate.sealed")
	if err := sealFile(file, kept, r.Token); err != nil {
		r.Log.Error("state the apply couldn't save is lost", "run", job.RunID, "err", err)
		return "the state couldn't be saved and is lost: the resources this apply changed may be untracked"
	}
	push := fmt.Sprintf("OPS_RUNNER_TOKEN=… opsolete unseal-state %s | terraform state push -", kept)
	r.Log.Error("state the apply couldn't save kept sealed on the runner", "run", job.RunID, "file", kept, "push", push)
	msg := fmt.Sprintf("the state couldn't be saved; it is kept sealed on runner %s: push it with %s", r.Name, push)
	fmt.Fprintf(w, "\n==> %s\n", msg)
	return msg
}

// erroredStateAD binds a sealed errored state to its use: another sealed value doesn't open as one.
const erroredStateAD = "opsolete errored state"

// sealFile writes src sealed with the runner token to dst (src stays: it's in the workdir, deleted after the job).
func sealFile(src, dst, token string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	k, err := secrets.New(token, "")
	if err != nil {
		return err
	}
	sealed, err := k.Seal(string(b), erroredStateAD)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, sealed, 0o600)
}

// UnsealState opens a state rescueState kept, with the token of the runner that kept it.
func UnsealState(sealed []byte, token string) (string, error) {
	k, err := secrets.New(token, "")
	if err != nil {
		return "", err
	}
	return k.Open(sealed, erroredStateAD)
}

// plan saves a plan; with changes it reports a summary and uploads the plan and the lock file
// for the apply phase.
func (r *Runner) plan(ctx context.Context, tf *tfProc, run func(...string) (bool, error), dir string, job runnerapi.Job, w io.Writer,
	fail func(string, error) runnerapi.FinishRequest) runnerapi.FinishRequest {
	destroy := job.Action == "destroy"
	refresh := job.Action == "refresh" || job.Action == "assess"
	// never applied, so they don't write state: no lock, they run in locked workspaces (storage.ClaimJob)
	readOnly := job.Action == "plan" || job.Action == "assess"
	var flags []string
	switch {
	case destroy:
		flags = append(flags, "-destroy")
	case refresh:
		flags = append(flags, "-refresh-only")
	}
	for _, a := range job.Replace {
		flags = append(flags, "-replace="+a)
	}
	for _, a := range job.Target {
		flags = append(flags, "-target="+a)
	}
	fmt.Fprintf(w, "\n==> terraform %s\n", strings.Join(append([]string{"plan"}, flags...), " "))
	// -var-file, not an .auto.tfvars file: a repository's own *.auto.tfvars must not override the workspace's
	// variables (Terraform reads auto files in name order, the last wins; -var-file beats them all), as in HCP
	changes, err := run(append([]string{"plan", "-input=false", "-detailed-exitcode", "-out=" + planFile, "-var-file=" + varsFile,
		"-lock=" + strconv.FormatBool(!readOnly)}, flags...)...)
	if err != nil {
		return fail("plan", err)
	}
	if !changes {
		fmt.Fprintln(w, "\n==> no changes")
		return runnerapi.FinishRequest{Status: "succeeded"}
	}
	p := &tfjson.Plan{}
	if err := tf.json(ctx, p, "show", "-json", "-no-color", planFile); err != nil {
		return fail("read plan", err)
	}
	rcs := p.ResourceChanges
	if refresh { // a refresh-only plan changes no resources; it lists the drift
		rcs = p.ResourceDrift
	}
	sum := summarize(rcs)
	diff := planDiff(rcs, mask.New(job.Secrets))
	var imported map[string]map[string]any
	if sum.Import > 0 { // what the imported resources are: an import's next round plans with it
		imported = found(p, job.Modules, mask.New(job.Secrets))
	}
	if !refresh { // a refresh-only plan changes no infrastructure, so no cost
		sum.Cost = r.estimate(ctx, dir, p, job.Currency, w)
	}
	switch job.Action { // never applied, so the plan isn't kept
	case "assess":
		fmt.Fprintf(w, "\n==> drift: %d changed, %d deleted outside Terraform\n", sum.Change, sum.Destroy)
		return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum, Diff: diff}
	case "plan":
		fmt.Fprintf(w, "\n==> speculative plan: %s (never applied)\n", counts(sum))
		return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum, Diff: diff, Found: imported}
	}
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
	fmt.Fprintf(w, "\n==> plan saved: %s\n", counts(sum))
	return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum, Diff: diff, Found: imported}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
