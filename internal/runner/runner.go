// Package runner executes Opsolete deployments with Terraform. It pulls jobs from the
// server's runner API, runs init/plan/apply (or a destroy plan) in a throwaway directory,
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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/runnerapi"
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
	if err := hardenProcess(); err != nil {
		return fmt.Errorf("harden runner process: %w", err)
	}
	r.Log.Info("runner started", "name", r.Name, "server", r.ServerURL.String())
	if os.Getpid() != 1 {
		r.Log.Warn("not running as a container's PID 1: processes a job leaves behind are not killed, so jobs aren't isolated from each other")
	}
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
	ship := newShipper(r, job, cancel, signed)
	res := r.terraform(ctx, job, src, ship)
	killJobProcesses()
	ship.Close()
	if ship.lost.Load() {
		log.Warn("lease lost; result not reported")
		return
	}
	switch {
	case res.Status == "succeeded": // finished before the cancel or shutdown took effect: keep the result
	case hard.Err() != nil:
		res.Status, res.Message = "canceled", "runner was shut down during the run"
	case ship.canceled.Load():
		res.Status, res.Message = "canceled", "canceled by a user; Terraform was interrupted and saved its state"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status, res.Message = "failed", fmt.Sprintf("timed out after %s", r.JobTimeout)
	}
	res.Message = ship.redact.Replace(res.Message) // Terraform's stderr may quote a signed URL
	if m := []rune(res.Message); len(m) > 2000 {   // after redacting: a cut must not split a signed URL
		res.Message = string(m[:2000]) + "…"
	}
	r.finish(log, job, res)
}

const (
	planFile    = "opsolete.tfplan"
	lockFile    = ".terraform.lock.hcl"
	modulesFile = "opsolete_templates.tf.json" // module blocks of the workspace's added templates
	outputsFile = "opsolete_outputs.tf.json"   // their outputs, as root outputs
)

// terraform runs one phase in a throwaway directory. plan: save a plan, summarize it and upload
// it with the provider lock file. apply: restore both, then apply exactly that plan.
func (r *Runner) terraform(ctx context.Context, job runnerapi.Job, src string, w io.Writer) (res runnerapi.FinishRequest) {
	fail := func(step string, err error) runnerapi.FinishRequest {
		fmt.Fprintf(w, "\n==> %s failed\n", step)
		return runnerapi.FinishRequest{Status: "failed", Message: step + ": " + err.Error()}
	}
	dir, err := os.MkdirTemp("", "ops-job-*")
	if err != nil {
		return fail("prepare workdir", err)
	}
	defer os.RemoveAll(dir)
	tf, err := tfexec.NewTerraform(dir, r.Terraform)
	if err != nil {
		return fail("prepare terraform", err)
	}
	// After a cancel's SIGINT, Terraform finishes in-flight operations and saves state: that can take far
	// longer than terraform-exec's 60s default before it kills the process (errors only on Windows).
	_ = tf.SetWaitDelay(r.JobTimeout)
	// terraform-exec only runs show and output (their output holds values); init, plan and apply
	// run via tfRun, which keeps Terraform's colors.
	tf.SetStdout(io.Discard)
	tf.SetStderr(io.Discard)
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
	// CleanEnv drops the few variables terraform-exec sets itself (TF_LOG, TF_INPUT, ...).
	if err := tf.SetEnv(tfexec.CleanEnv(env)); err != nil {
		return fail("set environment", err)
	}
	cmdEnv := []string{"TF_IN_AUTOMATION=1"} // what terraform-exec adds; CleanEnv cleaned env in place
	for k, v := range env {
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	debug, _ := strconv.ParseBool(job.Env["OPS_DEBUG"]) // a workspace env variable: full warnings
	run := func(args ...string) (bool, error) {
		if !debug && (args[0] == "plan" || args[0] == "apply") {
			// one line per warning, e.g. the -target warning of every template removal
			args = slices.Insert(args, 1, "-compact-warnings")
		}
		return r.tfRun(ctx, dir, cmdEnv, w, args...)
	}

	fmt.Fprintf(w, "==> runner %s: %s phase of %s run %s\n", r.Name, job.Phase, job.Action, job.RunID)
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
	if err := os.WriteFile(filepath.Join(dir, "opsolete.auto.tfvars.json"), vars, 0o600); err != nil {
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
		return r.plan(ctx, tf, run, dir, job, w, fail)
	}

	// The apply writes state: report what's in it however it ends, since a failed apply can
	// leave resources behind too.
	defer func() { res.Resources = inventory(ctx, tf) }()
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
	outs, err := tf.Output(ctx) // prints sensitive values in clear text: tf's output is discarded
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

// dropRemoved finishes removing templates after the destroy limited to them (-target=module.<name>):
// Terraform keeps all outputs on a targeted destroy, so it plans the configuration without those
// templates (their resources are gone, so their providers aren't needed anymore) and applies that
// plan if it only changes outputs. -refresh=false: no API calls, and drift can't sneak in.
func dropRemoved(ctx context.Context, tf *tfexec.Terraform, run func(...string) (bool, error), dir string, job runnerapi.Job, w io.Writer) error {
	left := slices.DeleteFunc(slices.Clone(job.Modules), func(m runnerapi.Module) bool { return slices.Contains(job.Target, "module."+m.Name) })
	for _, f := range []string{modulesFile, outputsFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if len(left) > 0 { // an empty module block list is invalid
		if err := writeModules(dir, left); err != nil {
			return err
		}
	}
	if err := writeOutputs(dir, left); err != nil {
		return err
	}
	const cleanupPlan = "opsolete-cleanup.tfplan"
	fmt.Fprintln(w, "\n==> terraform plan -refresh=false (without the removed templates, to drop their outputs)")
	changes, err := run("plan", "-input=false", "-detailed-exitcode", "-out="+cleanupPlan, "-refresh=false")
	if err != nil || !changes {
		return err
	}
	p, err := tf.ShowPlanFile(ctx, cleanupPlan) // includes values: tf's output is discarded
	if err != nil {
		return err
	}
	for _, rc := range p.ResourceChanges {
		if rc.Change != nil && !rc.Change.Actions.NoOp() && !rc.Change.Actions.Read() {
			return fmt.Errorf("the plan also changes %s: not applied", rc.Address)
		}
	}
	fmt.Fprintln(w, "\n==> terraform apply (outputs only)")
	_, err = run("apply", "-auto-approve", "-input=false", cleanupPlan)
	return err
}

// templateEscape keeps an input a value: strings in .tf.json are templates, so "${...}" in a form
// value would otherwise be evaluated (e.g. reading the runner's files or credentials).
var templateEscape = strings.NewReplacer("${", "$${", "%{", "%%{")

// escapeValue applies templateEscape to every string in v, nested ones and object keys included
// (HCL JSON evaluates property names as templates too).
func escapeValue(v any) any {
	switch v := v.(type) {
	case string:
		return templateEscape.Replace(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = escapeValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[templateEscape.Replace(k)] = escapeValue(e)
		}
		return out
	}
	return v
}

// writeModules writes a module block per added template, inputs as literal arguments.
func writeModules(dir string, mods []runnerapi.Module) error {
	blocks := map[string]map[string]any{}
	for _, m := range mods {
		b := map[string]any{}
		for k, v := range m.Inputs {
			b[k] = escapeValue(v)
		}
		b["source"] = m.Source // last: an input can't replace it
		blocks[m.Name] = b
	}
	j, err := json.Marshal(map[string]any{"module": blocks})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, modulesFile), j, 0o600)
}

// writeOutputs exposes every output of the added templates as root output <name>__<output>, with
// the module's sensitivity (Terraform requires it), so the run reports them like root outputs.
func writeOutputs(dir string, mods []runnerapi.Module) error {
	var manifest struct{ Modules []struct{ Key, Dir string } }
	b, err := os.ReadFile(filepath.Join(dir, ".terraform", "modules", "modules.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return err
	}
	outs := map[string]any{}
	for _, m := range manifest.Modules {
		if !slices.ContainsFunc(mods, func(a runnerapi.Module) bool { return a.Name == m.Key }) {
			continue
		}
		mod, diags := tfconfig.LoadModule(filepath.Join(dir, m.Dir))
		if diags.HasErrors() {
			return diags.Err()
		}
		for name, o := range mod.Outputs {
			outs[m.Key+"__"+name] = map[string]any{"value": "${module." + m.Key + "." + name + "}", "sensitive": o.Sensitive}
		}
	}
	if len(outs) == 0 {
		return nil
	}
	j, err := json.Marshal(map[string]any{"output": outs})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, outputsFile), j, 0o600)
}

// plan saves a plan; with changes it reports a summary and uploads the plan and the lock file
// for the apply phase.
func (r *Runner) plan(ctx context.Context, tf *tfexec.Terraform, run func(...string) (bool, error), dir string, job runnerapi.Job, w io.Writer,
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
	changes, err := run(append([]string{"plan", "-input=false", "-detailed-exitcode", "-out=" + planFile,
		"-lock=" + strconv.FormatBool(!readOnly)}, flags...)...)
	if err != nil {
		return fail("plan", err)
	}
	if !changes {
		fmt.Fprintln(w, "\n==> no changes")
		return runnerapi.FinishRequest{Status: "succeeded"}
	}
	p, err := tf.ShowPlanFile(ctx, planFile) // includes variable and attribute values: tf's output is discarded
	if err != nil {
		return fail("read plan", err)
	}
	rcs := p.ResourceChanges
	if refresh { // a refresh-only plan changes no resources; it lists the drift
		rcs = p.ResourceDrift
	}
	sum := summarize(rcs)
	if !refresh { // a refresh-only plan changes no infrastructure, so no cost
		sum.Cost = r.estimate(ctx, dir, p, job.Currency, w)
	}
	switch job.Action { // never applied, so the plan isn't kept
	case "assess":
		fmt.Fprintf(w, "\n==> drift: %d changed, %d deleted outside Terraform\n", sum.Change, sum.Destroy)
		return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum}
	case "plan":
		fmt.Fprintf(w, "\n==> speculative plan: %d to add, %d to change, %d to destroy (never applied)\n", sum.Add, sum.Change, sum.Destroy)
		return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum}
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
	fmt.Fprintf(w, "\n==> plan saved: %d to add, %d to change, %d to destroy\n", sum.Add, sum.Change, sum.Destroy)
	return runnerapi.FinishRequest{Status: "succeeded", Plan: &sum}
}

// estimate prices a plan with `infracost breakdown`. nil when Infracost isn't configured or
// fails: a missing estimate never fails the run.
func (r *Runner) estimate(ctx context.Context, dir string, p *tfjson.Plan, currency string, w io.Writer) *runnerapi.Cost {
	if r.InfracostAPIKey == "" {
		return nil
	}
	skip := func(err error) *runnerapi.Cost {
		fmt.Fprintf(w, "\n==> cost estimation skipped: %v\n", err)
		return nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return skip(err)
	}
	path := filepath.Join(dir, "opsolete.tfplan.json") // holds variable values: stays in the job's dir
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return skip(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Infracost, "breakdown", "--path", path, "--format", "json", "--log-level", "error")
	cmd.Dir = dir
	// Only what Infracost needs: none of the job's variables or credentials. HOME is the job's dir,
	// so its config and cache go away with it.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "INFRACOST_API_KEY=" + r.InfracostAPIKey,
		"INFRACOST_SKIP_UPDATE_CHECK=true", "INFRACOST_NO_COLOR=true"}
	if currency != "" {
		cmd.Env = append(cmd.Env, "INFRACOST_CURRENCY="+currency)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return skip(fmt.Errorf("infracost: %w: %s", err, msg))
	}
	var res struct {
		Currency                                                     string
		PastTotalMonthlyCost, TotalMonthlyCost, DiffTotalMonthlyCost *string // null without priced resources
		Summary                                                      struct{ TotalSupportedResources, TotalUnsupportedResources int }
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return skip(fmt.Errorf("read infracost output: %w", err))
	}
	c := &runnerapi.Cost{Currency: res.Currency, Priced: res.Summary.TotalSupportedResources, Unsupported: res.Summary.TotalUnsupportedResources}
	for _, f := range []struct {
		dst *float64
		src *string
	}{{&c.Prior, res.PastTotalMonthlyCost}, {&c.Proposed, res.TotalMonthlyCost}, {&c.Delta, res.DiffTotalMonthlyCost}} {
		if f.src == nil {
			continue
		}
		if *f.dst, err = strconv.ParseFloat(*f.src, 64); err != nil {
			return skip(fmt.Errorf("read infracost output: %w", err))
		}
	}
	fmt.Fprintf(w, "\n==> estimated monthly cost: %s %.2f (%+.2f)\n", c.Currency, c.Proposed, c.Delta)
	return c
}

// summarize counts a plan's resource changes (or drift), like Terraform's "Plan: x to add" line,
// and lists their addresses. Values are never included.
func summarize(rcs []*tfjson.ResourceChange) runnerapi.Plan {
	sum := runnerapi.Plan{Changes: []runnerapi.Change{}}
	for _, rc := range rcs {
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
	st, err := tf.Show(ctx) // prints the whole state, secrets included: tf's output is discarded
	if err != nil {
		return nil
	}
	return runnerapi.Inventory(st)
}

// backendOptions points Terraform's http backend at the workspace's state on the server. Its
// credentials come from TF_HTTP_USERNAME/TF_HTTP_PASSWORD (see terraform), so the token stays off the
// command line, .terraform/ and the saved plan. It only works while this run is planning or applying
// (module code can read it from the env): it reaches this workspace's state and, read-only, the states
// shared with it.
func (r *Runner) backendArgs(job runnerapi.Job) []string {
	addr := r.ServerURL.JoinPath(runnerapi.StatePrefix, job.WorkspaceID).String()
	return []string{"-backend-config=address=" + addr, "-backend-config=lock_address=" + addr, "-backend-config=unlock_address=" + addr}
}

// ansiCodes matches Terraform's color codes (SGR sequences).
var ansiCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

// tfRun runs a terraform command with its colors on, which terraform-exec can't do (it always adds
// -no-color); the run page renders them. Otherwise like terraform-exec: SIGINT on cancel, stderr in
// the error. With -detailed-exitcode, exit code 2 means "changes present", reported as true.
func (r *Runner) tfRun(ctx context.Context, dir string, env []string, w io.Writer, args ...string) (bool, error) {
	stderr := headBuffer{max: 64 << 10} // the job's message keeps only the start of it
	cmd := exec.CommandContext(ctx, r.Terraform, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, io.MultiWriter(w, &stderr)
	// After SIGINT, Terraform finishes in-flight operations and saves state before it exits.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = r.JobTimeout
	isolateCommand(cmd)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 && slices.Contains(args, "-detailed-exitcode") {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w\n%s", err, ansiCodes.ReplaceAllString(stderr.String(), ""))
	}
	return false, nil
}

// headBuffer keeps the first max bytes written to it and drops the rest.
type headBuffer struct {
	bytes.Buffer
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := h.max - h.Len(); room > 0 {
		h.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
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

// transferTimeout bounds an artifact upload or download, so a stalled connection doesn't hold the
// phase until the job timeout while heartbeats keep its lease.
// shortcut: a limit on the whole transfer, not on stalls; at 256 MB it needs about 0.5 MB/s.
const transferTimeout = 10 * time.Minute

// upload stores a plan phase's artifact on the server.
func (r *Runner) upload(ctx context.Context, job runnerapi.Job, name string, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	code, resp, err := r.call(ctx, "/jobs/"+job.RunID+"/artifacts/"+name, job.LeaseID, "application/octet-stream", b)
	if err == nil && code != http.StatusNoContent {
		err = fmt.Errorf("HTTP %d: %s", code, resp)
	}
	return err
}

// download writes an artifact of the run's plan phase to path; a missing optional one is skipped.
func (r *Runner) download(ctx context.Context, job runnerapi.Job, name, path string, required bool) error {
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, runnerapi.MaxJobSize+1))
	if err != nil {
		return 0, nil, err
	}
	if len(b) > runnerapi.MaxJobSize {
		return 0, nil, fmt.Errorf("%s: response is over %d bytes", path, runnerapi.MaxJobSize)
	}
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

func newShipper(r *Runner, job runnerapi.Job, cancel context.CancelFunc, signed []string) *shipper {
	var pairs []string
	for _, u := range signed {
		if i := strings.Index(u, "?"); i >= 0 {
			pairs = append(pairs, u[i:], "?<signed>")
		}
	}
	// A multi-line secret (a PEM key) is printed line by line too. Longest first: the replacer
	// tries its pairs in order, so a secret containing another is masked whole.
	var secrets []string
	for _, v := range job.Secrets {
		secrets = append(secrets, v)
		if strings.Contains(v, "\n") {
			secrets = append(secrets, strings.Split(v, "\n")...)
		}
	}
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, v := range slices.Compact(secrets) {
		if len(strings.TrimSpace(v)) >= 4 { // shortcut: shorter values (true, 1, eu) would mask ordinary output; they stay visible
			pairs = append(pairs, v, "***")
		}
	}
	s := &shipper{r: r, job: job, cancel: cancel, redact: strings.NewReplacer(pairs...),
		stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

func (s *shipper) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() < maxBuffered {
		s.buf.Write(p) // redacted in flush: a signed URL may arrive split over two writes
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
			// drain the tail (where errors are) in 256 KB posts; stop at the first failure
			for s.flush(true) {
			}
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

// flush sends buffered output; unless final, only up to the last newline. Reports whether it sent
// something successfully.
func (s *shipper) flush(final bool) bool {
	s.mu.Lock()
	data := s.buf.Bytes()
	n := len(data)
	if !final {
		n = bytes.LastIndexByte(data, '\n') + 1
	}
	if n > 256<<10 { // cut at a line end if there is one, so redaction sees whole lines
		n = 256 << 10
		if i := bytes.LastIndexByte(data[:n], '\n'); i >= 0 {
			n = i + 1
		}
	}
	chunk := bytes.Clone(data[:n])
	s.buf.Next(n)
	s.mu.Unlock()
	if len(chunk) == 0 {
		return false
	}
	chunk = []byte(s.redact.Replace(string(chunk)))
	if !s.post("/logs", chunk) {
		if s.lost.Load() {
			return false
		}
		s.mu.Lock() // keep it for the next attempt
		rest := bytes.Clone(s.buf.Bytes())
		s.buf.Reset()
		s.buf.Write(chunk)
		s.buf.Write(rest)
		s.mu.Unlock()
		return false
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
