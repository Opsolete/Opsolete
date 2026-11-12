package app

// The runner protocol: runners claim a run's phase as a job, keep its lease alive, post its log,
// ship its artifacts and finish it. Runners authenticate with their own token (RunnerByToken, checked by
// the surface) and are named, not Actors; a job's lease id is their credential for it.

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"strings"
	"time"

	"github.com/google/uuid"
	"gocloud.dev/gcerrors"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

const (
	LeaseTTL  = 2 * time.Minute  // runners heartbeat well within this
	AckWindow = 30 * time.Second // a claim's lease until acknowledged: enough for a slow network, short against LeaseTTL
	// ReapGrace: how long a new leader waits before failing expired leases. A live runner heartbeats
	// within it after an outage: every LeaseTTL/4 (30 s) when idle, after a post that may hang up to 15 s.
	ReapGrace       = time.Minute
	MaxLogBytes     = 10 << 20         // per run
	MaxArtifactSize = 256 << 20        // saved plans embed the configuration; generous
	runnerSrcTTL    = 15 * time.Minute // signed archive URL handed to the runner; it downloads right away
	driftInterval   = 24 * time.Hour   // how often workspaces with drift detection are assessed
)

// errNoArtifact: not an artifact name of the protocol, or none was stored.
var errNoArtifact = reason{ErrNotFound, "no such artifact"}

// StateToken is a run's state password. It is per run, not per phase: Terraform embeds the backend
// config in the saved plan, so the apply phase authenticates with what the plan phase used.
func (a *App) StateToken(workspaceID, runID uuid.UUID) string {
	m := hmac.New(sha256.New, a.Registry.Key)
	m.Write([]byte("opsolete state\x00" + workspaceID.String() + "\x00" + runID.String()))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ClaimJob hands runner the next queued job, JSON-encoded (at most runnerapi.MaxJobSize), or
// ok=false if there is none. The runner must acknowledge it (AckJob) within AckWindow, or
// ReapLeases queues it again: the response may never have reached the runner. A claimed job that can't be assembled (variables that don't open, no
// configuration, VCS unreachable, too big) fails at once rather than waiting for the lease reaper,
// and the next one is tried.
func (a *App) ClaimJob(ctx context.Context, runner storage.Runner) (body []byte, ok bool, err error) {
	for {
		j, ok, err := a.Store.ClaimJob(ctx, runner, AckWindow)
		if err != nil || !ok {
			return nil, false, err
		}
		body, err := a.assembleJob(ctx, j)
		if err == nil {
			a.Log.Info("job claimed", "run", j.RunID, "phase", j.Phase, "action", j.Action, "runner", runner.Name, "pool", runner.Pool)
			return body, true, nil
		}
		a.Log.Error("job variables", "run", j.RunID, "err", err)
		// not canceled with the long poll: the job is claimed, it must end. The server acknowledges it
		// itself (no runner will), as FinishJob needs.
		fctx := context.WithoutCancel(ctx)
		ferr := a.Store.AckJob(fctx, j.RunID, j.LeaseID, LeaseTTL)
		if ferr == nil {
			_, ferr = a.Store.FinishJob(fctx, j.RunID, j.LeaseID, "failed", err.Error(), nil, nil, nil, nil)
		}
		if ferr != nil {
			a.Log.Error("fail job", "run", j.RunID, "err", ferr)
		} else {
			a.runEnded(ctx, j.RunID) // "failed" always ends it
		}
	}
}

// assembleJob opens what the runner needs for j: variables, template inputs and the configuration's source.
func (a *App) assembleJob(ctx context.Context, j storage.Job) ([]byte, error) {
	env, vars, secrets, err := a.runVariables(ctx, j.WorkspaceID)
	var src, currency string
	if err == nil {
		src, err = a.jobSource(ctx, j)
	}
	if err == nil {
		currency, err = a.Store.Currency(ctx)
	}
	var envValues EnvironmentMap // the workspace's project now: a moved workspace gets its new project's values
	if err == nil {
		envValues, err = environmentMap(a.Store.WorkspaceEnvironmentValues(ctx, j.WorkspaceID))
	}
	var mods []runnerapi.Module
	for _, m := range j.Modules {
		in, sec, oerr := a.OpenInputs(m.Inputs, j.WorkspaceID, m.Name)
		err, secrets = cmp.Or(err, oerr), append(secrets, sec...)
		if err == nil {
			in, err = a.withEnvironmentValues(ctx, m.TemplateID, in, envValues)
		}
		if err == nil {
			in, err = a.withWorkspaceVars(ctx, m.TemplateID, m.ModuleSource, m.ModuleRef, in, vars)
		}
		mods = append(mods, runnerapi.Module{Name: m.Name, Inputs: in,
			Source: a.Registry.RunnerSource(templates.Module{Source: m.ModuleSource, Ref: m.ModuleRef}, runnerSrcTTL)})
	}
	if err != nil {
		return nil, err
	}
	var reg *runnerapi.RegistryAccess
	if a.RegistryEnabled && a.Registry.PublicURL != nil {
		reg = &runnerapi.RegistryAccess{Host: a.Registry.PublicURL.Host, Token: a.RegistryToken(j.RunID)}
		secrets = append(secrets, reg.Token) // masked in the log, like the state password
	}
	body, err := json.Marshal(runnerapi.Job{
		RunID: j.RunID.String(), WorkspaceID: j.WorkspaceID.String(), Phase: j.Phase, Action: j.Action, Replace: j.Replace, Target: j.Target, LeaseID: j.LeaseID,
		LeaseSeconds: int(LeaseTTL.Seconds()), StateToken: a.StateToken(j.WorkspaceID, j.RunID), Vars: vars, Env: env, Secrets: secrets,
		ModuleSource: src, Modules: mods, Currency: currency, Registry: reg,
	})
	if err == nil && len(body) > runnerapi.MaxJobSize {
		err = fmt.Errorf("the job's variables and inputs are %d bytes, over the runner limit of %d", len(body), runnerapi.MaxJobSize)
	}
	return body, err
}

// jobSource is where the runner fetches the run's root configuration: this server's archive of the
// VCS commit the run plans (pinned at the plan phase, reused by the apply), or none when added
// templates are the whole configuration.
func (a *App) jobSource(ctx context.Context, j storage.Job) (string, error) {
	switch {
	case j.VCS:
		if j.Phase == "plan" {
			if err := a.pinCommit(ctx, j); err != nil {
				return "", err
			}
		}
		return a.Registry.SignedArchiveURL(VCSArchivePath(j.RunID), runnerSrcTTL), nil
	case len(j.Modules) > 0:
		return "", nil
	}
	return "", errors.New("the workspace has no configuration: add a template or link a repository")
}

// AckJob: the runner holding lease received the job and starts it; its lease becomes LeaseTTL.
// Every other job call needs this first. storage.ErrLeaseLost: the lease is gone (unclaimed, canceled).
func (a *App) AckJob(ctx context.Context, runID uuid.UUID, lease string) error {
	return a.Store.AckJob(ctx, runID, lease, LeaseTTL)
}

// Heartbeat extends a job's lease; cancel asks the runner to stop gracefully (it keeps its lease,
// so Terraform can still save state). storage.ErrLeaseLost (a conflict): the lease is gone.
func (a *App) Heartbeat(ctx context.Context, runID uuid.UUID, lease string) (cancel bool, err error) {
	return a.Store.Heartbeat(ctx, runID, lease, LeaseTTL)
}

// AppendLog stores a chunk of the job's output (made valid UTF-8 without NULs) and extends its
// lease; cancel and errors as for Heartbeat.
func (a *App) AppendLog(ctx context.Context, runID uuid.UUID, lease string, chunk []byte) (cancel bool, err error) {
	text := strings.ReplaceAll(strings.ToValidUTF8(string(chunk), "\uFFFD"), "\x00", "\uFFFD")
	return a.Store.AppendLog(ctx, runID, lease, text, LeaseTTL, MaxLogBytes)
}

// FinishJob records a job's result and returns the run's new status (storage.FinishJob), cleaning
// up if that ended the run. ValidationError: the result's status.
func (a *App) FinishJob(ctx context.Context, runID uuid.UUID, lease string, req runnerapi.FinishRequest) (string, error) {
	if req.Status != "succeeded" && req.Status != "failed" && req.Status != "canceled" {
		return "", invalid("status", "status must be succeeded, failed or canceled")
	}
	if len(req.Outputs) > 0 { // the runner masks them too; the server is the trust boundary, so again here
		req.Outputs = a.maskRunOutputs(ctx, runID, req.Outputs)
	}
	for mod, in := range req.Found { // form values: one holding a workspace secret is dropped, as the runner does
		masked := a.maskRunOutputs(ctx, runID, in)
		for k, v := range in {
			if !reflect.DeepEqual(masked[k], v) {
				delete(in, k)
			}
		}
		if len(in) == 0 {
			delete(req.Found, mod)
		}
	}
	// not canceled with the request: a job's result must survive a server restart
	finish := func(ctx context.Context) (string, error) {
		return a.Store.FinishJob(ctx, runID, lease, req.Status, req.Message, req.Outputs, req.Plan, req.Resources, req.Found)
	}
	status, err := finish(context.WithoutCancel(ctx))
	if err != nil {
		return "", err
	}
	a.Log.Info("job finished", "run", runID, "result", req.Status, "status", status)
	if storage.Ended(status) {
		a.runEnded(ctx, runID)
	}
	return status, nil
}

// artifactAccess checks name is an artifact of the protocol and lease holds the run in phase status.
func (a *App) artifactAccess(ctx context.Context, runID uuid.UUID, lease, name, phase string) error {
	if name != runnerapi.ArtifactPlan && name != runnerapi.ArtifactLockfile {
		return errNoArtifact
	}
	ok, err := a.Store.HoldsLease(ctx, runID, lease, phase)
	if err == nil && !ok {
		err = storage.ErrLeaseLost
	}
	return err
}

// PutArtifact stores a plan phase's output (a saved plan or lock file); only the planning run may
// write its own artifacts. storage.ErrLeaseLost: it doesn't (any more).
func (a *App) PutArtifact(ctx context.Context, runID uuid.UUID, lease, name string, body []byte) error {
	if err := a.artifactAccess(ctx, runID, lease, name, storage.Planning); err != nil {
		return err
	}
	return a.writeArtifact(context.WithoutCancel(ctx), runID, name, body) // a write that happened is checked, whatever the request does
}

// writeArtifact writes, then checks the run again: one that ended between the lease check and the
// write had its artifacts deleted (runEnded) before this one landed, so it's deleted here; a run
// ending after this check deletes it itself. Checking after the write needs no lock across the upload.
func (a *App) writeArtifact(ctx context.Context, runID uuid.UUID, name string, body []byte) error {
	key := ArtifactKey(runID, name)
	if err := a.WriteBlob(ctx, key, body); err != nil {
		return err
	}
	run, err := a.Store.RunStatus(ctx, runID)
	if err == nil && !storage.Ended(run.Status) {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := a.Blobs.Delete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		a.Log.Warn("delete artifact", "run", runID, "name", name, "err", err)
	}
	return storage.ErrLeaseLost
}

// Artifact hands a run's saved plan or lock file to its apply phase; ErrNotFound if there is none.
func (a *App) Artifact(ctx context.Context, runID uuid.UUID, lease, name string) ([]byte, error) {
	if err := a.artifactAccess(ctx, runID, lease, name, storage.Applying); err != nil {
		return nil, err
	}
	b, err := a.ReadBlob(ctx, ArtifactKey(runID, name))
	if gcerrors.Code(err) == gcerrors.NotFound {
		return nil, errNoArtifact
	}
	return b, err
}

// ReapLeases queues again claimed jobs their runner never acknowledged (storage.UnclaimJobs), and
// fails acknowledged jobs whose runner stopped heartbeating (crash, node loss) and cleans up after
// them, but not within ReapGrace of this replica taking the lead. For a background loop: errors are logged.
func (a *App) ReapLeases(ctx context.Context) {
	unclaimed, err := a.Store.UnclaimJobs(ctx)
	if err != nil {
		a.Log.Error("unclaim jobs", "err", err)
	}
	for _, u := range unclaimed {
		// a runner whose claims keep showing up here never acknowledges: an old or broken runner
		a.Log.Warn("job not acknowledged by its runner, queued again", "run", u.RunID, "runner", u.Runner)
	}
	// Leases that ran out while no replica led (a server or database outage) aren't the runners' fault:
	// failing those runs now would refuse their applies' state writes and orphan what they created.
	// A new leader gives runners ReapGrace to heartbeat again (Heartbeat renews an expired lease).
	if since, ok := leadingSince(ctx); ok && time.Since(since) < ReapGrace {
		return
	}
	ids, err := a.Store.ReapExpiredLeases(ctx)
	if err != nil {
		a.Log.Error("reap expired leases", "err", err)
	}
	for _, id := range ids {
		a.Log.Warn("runner lost, deployment failed", "id", id)
		a.runEnded(ctx, id)
	}
}

// QueueDriftChecks queues the drift checks that are due (storage.QueueAssessments). For a
// background loop: errors are logged.
func (a *App) QueueDriftChecks(ctx context.Context) {
	if n, err := a.Store.QueueAssessments(ctx, driftInterval); err != nil {
		a.Log.Error("queue drift checks", "err", err)
	} else if n > 0 {
		a.Log.Info("drift checks queued", "count", n)
	}
}

// maskRunOutputs hides outputs that contain a sensitive value of the run's workspace, as saveState
// does for state files; if the secrets can't be opened, every output is hidden (the result is kept).
func (a *App) maskRunOutputs(ctx context.Context, runID uuid.UUID, outputs map[string]any) map[string]any {
	run, _, err := a.Store.GetRun(ctx, runID)
	var secrets mask.Masker
	if err == nil {
		secrets, err = a.workspaceMasker(ctx, run.WorkspaceID)
	}
	out := make(map[string]any, len(outputs))
	for k, v := range outputs {
		out[k] = secrets.Value(v)
		if err != nil {
			out[k] = mask.Sensitive
		}
	}
	if err != nil {
		a.Log.Error("open workspace secrets to mask outputs; all outputs hidden", "run", runID, "err", err)
	}
	return out
}
