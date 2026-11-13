package storage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
)

// ErrLeaseLost means the caller no longer holds the job: it expired, finished or was never claimed.
var ErrLeaseLost = errors.New("job lease lost")

// Job is one phase of a run, as handed to a runner.
type Job struct {
	RunID       uuid.UUID
	WorkspaceID uuid.UUID
	Phase       string // plan | apply
	Action      string // apply | destroy | refresh | assess | plan
	Replace     []string
	Target      []string // -target: module.<name> of each template a destroy run removes
	LeaseID     string
	VCS         bool                // the root configuration comes from the workspace's VCS branch
	VCSCommit   string              // commit pinned when the run was queued (VCS-triggered runs), or by an earlier phase
	Modules     []WorkspaceTemplate // templates added to the workspace
}

// QueuedJobs is how many phases runners of pool could claim now (ClaimJob's choice: not those waiting for a
// workspace's state lock).
func (s *Store) QueuedJobs(ctx context.Context, pool string) (int, error) {
	n, err := s.q.ClaimableRuns(ctx, pool)
	return int(n), err
}

// ClaimJob hands the longest-waiting queued phase (plan or confirmed apply) of a workspace whose project uses
// runner's pool to runner, or returns ok=false if there is none. SKIP LOCKED lets any number of runners claim without handing out a
// job twice. Runs of a locked workspace (a lock from the UI, the CLI or a rollback) wait until it
// is unlocked; speculative plans and drift checks, never applied, go ahead
// without taking the state lock.
func (s *Store) ClaimJob(ctx context.Context, runner Runner, lease time.Duration) (Job, bool, error) {
	j := Job{LeaseID: rand.Text()}
	var ok bool
	err := s.tx(ctx, func(q *db.Queries) error {
		next, err := q.ClaimableRun(ctx, runner.Pool)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		j.RunID, j.Phase = next.ID, "plan"
		status := map[string]string{PlanQueued: Planning, ApplyQueued: Applying}[next.Status]
		if status == Applying {
			j.Phase = "apply"
		}
		if _, err := setStatus(ctx, q, j.RunID, EvClaim, status, j.Phase+" claimed by runner "+runner.Name); err != nil {
			return err
		}
		r, err := q.LeaseRun(ctx, db.LeaseRunParams{ID: j.RunID, Runner: runner.Name, LeaseID: j.LeaseID, LeaseSecs: lease.Seconds()})
		if err != nil {
			return err
		}
		j.WorkspaceID, j.Action, j.VCSCommit = r.WorkspaceID, r.Action, r.VcsCommit
		if r.Replace != nil {
			if err := json.Unmarshal(r.Replace, &j.Replace); err != nil {
				return err
			}
		}
		for _, m := range r.RemoveTemplates {
			j.Target = append(j.Target, "module."+m)
		}
		if j.VCS, err = q.WorkspaceHasVCS(ctx, j.WorkspaceID); err != nil {
			return err
		}
		if j.Modules, err = configModules(ctx, q, r.CvID); err != nil { // the run's version, both phases
			return err
		}
		ok = true
		return nil
	})
	if err != nil || !ok {
		return Job{}, false, err
	}
	return j, true, nil
}

// AckJob records that the runner holding lease received the job and extends the lease to ttl. Until
// then the job can't be used (heartbeat, log, finish, artifacts, state: ErrLeaseLost) and UnclaimJobs
// may hand it to another runner. Acknowledging again is harmless. ErrLeaseLost: the lease is gone.
func (s *Store) AckJob(ctx context.Context, runID uuid.UUID, lease string, ttl time.Duration) error {
	n, err := s.q.AckLease(ctx, db.AckLeaseParams{ID: runID, LeaseID: lease, TtlSecs: ttl.Seconds()})
	if err == nil && n == 0 {
		err = ErrLeaseLost
	}
	return err
}

// Heartbeat extends the lease of a running job. cancel reports that a user asked to cancel it.
func (s *Store) Heartbeat(ctx context.Context, runID uuid.UUID, lease string, ttl time.Duration) (cancel bool, err error) {
	cancel, err = s.q.Heartbeat(ctx, db.HeartbeatParams{ID: runID, LeaseID: lease, TtlSecs: ttl.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	return cancel, err
}

// HoldsLease reports whether lease is the current lease of a run in the given phase status.
func (s *Store) HoldsLease(ctx context.Context, runID uuid.UUID, lease, status string) (bool, error) {
	return s.q.HoldsLease(ctx, db.HoldsLeaseParams{ID: runID, LeaseID: lease, Status: status})
}

// AppendLog stores a chunk of job output and extends the lease. Past maxBytes the log is
// truncated (with one marker line) but the job keeps running. cancel is as for Heartbeat.
func (s *Store) AppendLog(ctx context.Context, runID uuid.UUID, lease, chunk string, ttl time.Duration, maxBytes int64) (cancel bool, err error) {
	err = s.tx(ctx, func(q *db.Queries) error {
		r, err := q.LeaseLogBytes(ctx, db.LeaseLogBytesParams{ID: runID, LeaseID: lease, TtlSecs: ttl.Seconds(), Bytes: int64(len(chunk))})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		cancel = r.CancelRequested
		before := r.LogBytes - int64(len(chunk))
		switch {
		case r.LogBytes <= maxBytes:
		case before <= maxBytes:
			chunk = "\n[opsolete: log truncated, limit reached]\n"
		default:
			return nil
		}
		if err := q.InsertLog(ctx, db.InsertLogParams{RunID: runID, Chunk: chunk}); err != nil {
			return err
		}
		return notifyRun(ctx, q, runID)
	})
	if err != nil {
		return false, err
	}
	return cancel, nil
}

// CancelRun cancels a queued run at once. A run a runner works on is asked to stop: the runner
// interrupts Terraform, which still saves state, and reports it canceled. force cancels it right
// away instead and fences the runner off (its state password dies with the run), for runners
// that don't react; resources being created at that moment may then be missing from state.
// It returns the run's new status.
func (s *Store) CancelRun(ctx context.Context, id uuid.UUID, email string, force bool) (string, error) {
	var status string
	err := s.tx(ctx, func(q *db.Queries) error {
		run, err := q.LockRunStatus(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotCancelable
		}
		if err != nil {
			return err
		}
		status = run.Status
		busy := status == Planning || status == Applying
		if busy && force && !run.CancelRequested { // ask Terraform to stop first, cut it off only if it doesn't
			return ErrForceFirst
		}
		if busy && !force {
			if err := q.RequestCancel(ctx, id); err != nil {
				return err
			}
			// a note, not a transition: the run ends when its runner reports it canceled
			return addHistory(ctx, q, id, status, "cancel requested by "+email+": waiting for Terraform to stop and save state")
		}
		msg := "canceled by " + email
		if busy {
			msg = "force-canceled by " + email + "; the runner was cut off, check the state for resources it may have missed"
		}
		if _, err := setStatus(ctx, q, id, EvCancel, Canceled, msg); errors.Is(err, ErrTransition) {
			return ErrNotCancelable
		} else if err != nil {
			return err
		}
		status = Canceled
		return nil
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// FinishJob records the result of a phase, releases the lease and the workspace's state lock, and
// returns the run's new status:
//   - plan with changes: needs_confirmation, or apply_queued for apply runs of auto-apply workspaces
//     (destroys, refresh-only runs, template rollouts and plans that import into a template always wait for a
//     human), or drifted for assess runs and planned for speculative plans, which are never applied; without
//     changes: no_changes
//   - plan of an apply run that imports a resource a state manages already (resources: this workspace's state as
//     the plan read it; the index for the others), or that imports into a template and would replace or delete
//     anything: errored, and the template change is undone like any run that ends before its apply
//   - apply: applied; the templates it imported into stop importing (WorkspaceTemplate.Import false)
//   - failed / canceled: errored / canceled
//
// resources is the index of the workspace's current state version; found what a plan that imports found
// (runnerapi.FinishRequest.Found), kept for the form's "Use found values".
func (s *Store) FinishJob(ctx context.Context, runID uuid.UUID, lease, result, message string, outputs map[string]any,
	plan *runnerapi.Plan, resources []runnerapi.Resource, found map[string]map[string]any, diff []runnerapi.ResourceDiff) (string, error) {
	var final string
	var entry *AuditEntry
	err := s.tx(ctx, func(q *db.Queries) error {
		// the lease is checked here; setStatus then checks the transition
		r, err := q.LockLeasedRun(ctx, db.LockLeasedRunParams{ID: runID, LeaseID: lease})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		final = map[string]string{"failed": Errored, "canceled": Canceled}[result]
		// a template's module importing what exists: always a human's call (a VCS configuration's own import
		// blocks are its authors' business, but no resource may be in two states either way)
		rc, err := q.RunConfig(ctx, runID)
		if err != nil {
			return err
		}
		mods, err := configModules(ctx, q, rc.CvID) // the templates the run plans
		if err != nil {
			return err
		}
		var importing bool
		var conflict string
		// an import's plan rounds (action plan) and its apply: no resource in two states
		if r.Status == Planning && (r.Action == "apply" || r.Action == "plan") && plan != nil && len(plan.Imports) > 0 && final == "" {
			if conflict, err = importConflict(ctx, q, plan.Imports, resources); err != nil {
				return err
			}
			names := importedTemplates(mods, plan.Imports)
			importing = len(names) > 0
		}
		var ev Event
		var reason string
		switch {
		case final == Errored:
			ev = EvFail
		case final == Canceled:
			ev = EvCancel
		case r.Status == Applying:
			final, ev = Applied, EvApplied
			if r.Action == "apply" {
				if err := endImport(ctx, q, runID, r.WorkspaceID); err != nil {
					return err
				}
			}
		case conflict != "":
			final, message, ev = Errored, conflict, EvFail
		case importing && r.Action == "apply" && plan.Destroy > 0: // the template doesn't fit what exists (a plan round just shows it)
			final, message, ev = Errored, "import refused: the plan would replace or delete existing resources, so the template's "+
				"inputs don't match them; nothing was applied", EvFail
		default:
			ev = EvPlanned
			final, reason = afterPlan(PlanContext{Action: r.Action, AutoApply: r.AutoApply, Rollout: r.InRollout, Promotion: r.InPromotion,
				Importing: importing}, plan != nil)
		}
		// The plan finished before a cancel reached Terraform: end the run here, or the request still
		// standing would interrupt its apply.
		if r.CancelRequested && (final == NeedsConfirmation || final == ApplyQueued) {
			final, message, ev, reason = Canceled, "canceled: the plan finished before the cancel request reached the runner", EvCancel, ""
		}
		if reason != "" {
			if err := q.SetConfirmReason(ctx, db.SetConfirmReasonParams{ID: runID, Reason: reason}); err != nil {
				return err
			}
		}
		var planJSON, outJSON, foundJSON []byte
		if plan != nil {
			if planJSON, err = json.Marshal(plan); err != nil {
				return err
			}
		}
		if final == Applied && r.Action != "destroy" {
			if outJSON, err = json.Marshal(outputs); err != nil {
				return err
			}
		}
		if len(found) > 0 { // with each module's template: the workspace may not have the module (yet)
			byModule := map[string]Found{}
			for _, m := range mods {
				if in := found[m.Name]; len(in) > 0 {
					byModule[m.Name] = Found{TemplateID: m.TemplateID, Inputs: in}
				}
			}
			if foundJSON, err = json.Marshal(byModule); err != nil {
				return err
			}
		}
		if final == ApplyQueued && message == "" {
			message = "auto-apply: waiting for a runner"
		}
		if foundJSON != nil {
			if err := q.SetRunFound(ctx, db.SetRunFoundParams{ID: runID, Found: foundJSON}); err != nil {
				return err
			}
		}
		if diff != nil && r.Status == Planning {
			b, err := json.Marshal(diff)
			if err != nil {
				return err
			}
			if err := q.SetRunDiff(ctx, db.SetRunDiffParams{ID: runID, PlanDiff: b}); err != nil {
				return err
			}
		}
		_, step, err := transition(ctx, q, runID, ev, final, message)
		if err != nil {
			return err
		}
		if step != nil { // a promotion's apply run, in the name of who promoted
			e, err := s.audit(ctx, q, step.Actor, "promote apply queued", map[string]any{"source": step.Source,
				"workspace_id": r.WorkspaceID, "run": step.Run.ID, "after": step.After})
			if err != nil {
				return err
			}
			entry = &e
		}
		if err := q.SetRunResult(ctx, db.SetRunResultParams{ID: runID, Plan: planJSON, Outputs: outJSON}); err != nil {
			return err
		}
		if resources != nil {
			return indexState(ctx, q, r.WorkspaceID, resources)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if entry != nil {
		entry.stream()
	}
	return final, nil
}

// importedTemplates are the templates of mods whose modules imports go into.
func importedTemplates(mods []WorkspaceTemplate, imports []runnerapi.Import) []string {
	var names []string
	for _, m := range mods {
		if slices.ContainsFunc(imports, func(im runnerapi.Import) bool { return strings.HasPrefix(im.To, "module."+m.Name+".") }) {
			names = append(names, m.Name)
		}
	}
	return names
}

// endImport switches importing off in the templates applied run runID imported into, in a new version the
// workspace takes at once: what they imported is in the state, so their modules stop looking for it (a resource
// deleted outside Terraform later is created again, not looked for).
func endImport(ctx context.Context, q *db.Queries, runID, workspaceID uuid.UUID) error {
	raw, err := q.RunPlan(ctx, runID)
	if err != nil || raw == nil {
		return err
	}
	var plan runnerapi.Plan
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan.Imports) == 0 {
		return err
	}
	cur, mods, err := currentConfig(ctx, q, workspaceID) // the run's: its apply started
	if err != nil {
		return err
	}
	changed := false
	for i, m := range mods {
		if m.Import && slices.Contains(importedTemplates(mods, plan.Imports), m.Name) {
			mods[i].Import, changed = false, true
		}
	}
	if !changed {
		return nil
	}
	cv, err := newVersion(ctx, q, workspaceID, cur, mods, SourceImport, uuid.Nil)
	if err != nil {
		return err
	}
	return setCurrent(ctx, q, workspaceID, cv)
}

// ApplyImportConflict checks again, when run runID's apply is claimed, that no other state took a resource its
// plan imports since the plan ran (two workspaces may plan the same import); "" none.
func (s *Store) ApplyImportConflict(ctx context.Context, runID uuid.UUID) (string, error) {
	raw, err := s.q.RunPlan(ctx, runID)
	if err != nil || raw == nil {
		return "", notFound(err)
	}
	var plan runnerapi.Plan
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan.Imports) == 0 {
		return "", err
	}
	return importConflict(ctx, s.q, plan.Imports, nil)
}

// importConflict says which of a plan's imports a state manages already ("" none): this workspace's as its plan
// read it (resources; nil: its index), the others' by their index. A resource in two states, or twice in one,
// would be destroyed by removing either.
func importConflict(ctx context.Context, q *db.Queries, imports []runnerapi.Import, resources []runnerapi.Resource) (string, error) {
	ids := make([]string, len(imports))
	for i, im := range imports {
		ids[i] = strings.ToLower(im.ID)
		if ids[i] == "" { // nothing to check it against: it could be in another state already
			return "import refused: Terraform reported no id for " + im.To + ", so Opsolete can't check that no other state manages it; nothing was applied", nil
		}
		if slices.Index(ids, ids[i]) < i {
			return "import refused: " + im.ID + " is imported twice in this plan (" + imports[slices.Index(ids, ids[i])].To + " and " + im.To + "); nothing was applied", nil
		}
	}
	for _, res := range resources {
		if slices.Contains(ids, strings.ToLower(res.ID)) && res.ID != "" {
			return "import refused: " + res.ID + " is managed already, by " + res.Address + " in this workspace; nothing was applied", nil
		}
	}
	rows, err := q.ManagedResourceIDs(ctx, ids)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return "import refused: " + rows[0].ID + " is managed already, in workspace " + rows[0].Workspace + "; nothing was applied", nil
}

// Unclaimed is a job UnclaimJobs put back in the queue.
type Unclaimed struct {
	RunID    uuid.UUID
	Runner   string // the runner that claimed it and never acknowledged it
	Canceled bool   // a cancel was asked meanwhile: it ended canceled instead of queued again
}

// UnclaimJobs puts claimed jobs whose runner didn't acknowledge them within their claim's lease (the
// ack window, see ClaimJob's lease) back in the queue: planning → plan_queued, applying →
// apply_queued; setStatus ends the lease and wakes claimers. Nothing ran yet (a runner acknowledges
// before it starts), so this is safe; an acknowledged job is never unclaimed (ReapExpiredLeases).
func (s *Store) UnclaimJobs(ctx context.Context) ([]Unclaimed, error) {
	var out []Unclaimed
	err := s.tx(ctx, func(q *db.Queries) error {
		rows, err := q.UnackedLeases(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if run, err := q.LockRunStatus(ctx, r.ID); err != nil {
				return err
			} else if run.CancelRequested { // canceled before any runner started it: nothing ran, nothing to stop
				if _, err := setStatus(ctx, q, r.ID, EvCancel, Canceled, "canceled before a runner started it"); err != nil {
					return err
				}
				if err := q.ClearRunner(ctx, r.ID); err != nil {
					return err
				}
				out = append(out, Unclaimed{RunID: r.ID, Runner: r.Runner, Canceled: true})
				continue
			}
			queued := map[string]string{Planning: PlanQueued, Applying: ApplyQueued}[r.Status]
			if _, err := setStatus(ctx, q, r.ID, EvUnclaim, queued, "not acknowledged by runner "+r.Runner+": queued again"); err != nil {
				return err
			}
			if err := q.ClearRunner(ctx, r.ID); err != nil {
				return err
			}
			out = append(out, Unclaimed{RunID: r.ID, Runner: r.Runner})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReapExpiredLeases errors runs whose runner stopped heartbeating (setStatus releases their state
// locks) and returns their ids. Runs are not retried: a half-applied terraform run needs a human to
// look at it. SKIP LOCKED: a run being finished or canceled right now isn't waited for.
func (s *Store) ReapExpiredLeases(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.tx(ctx, func(q *db.Queries) error {
		var err error
		if ids, err = q.ExpiredLeases(ctx); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := setStatus(ctx, q, id, EvFail, Errored, "runner stopped responding (lease expired)"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

type LogChunk struct {
	Seq   int64
	Chunk string
}

// Logs returns up to limit chunks of a run's log after seq.
func (s *Store) Logs(ctx context.Context, runID uuid.UUID, after int64, limit int) ([]LogChunk, error) {
	rows, err := s.q.Logs(ctx, db.LogsParams{RunID: runID, After: after, Lim: int32(limit)})
	out := make([]LogChunk, 0, len(rows))
	for _, c := range rows {
		out = append(out, LogChunk{Seq: c.Seq, Chunk: c.Chunk})
	}
	return out, err
}

// Stats is what the metrics endpoint reports from the database.
type Stats struct {
	Runs         map[string]int // runs by status, all time
	OldestQueued time.Duration  // age of the oldest queued job, 0 without one
	Workspaces   int
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Runs: map[string]int{}}
	row, err := s.q.Stats(ctx)
	if err != nil {
		return st, err
	}
	st.Workspaces, st.OldestQueued = int(row.Workspaces), time.Duration(row.OldestQueued*float64(time.Second))
	rows, err := s.q.RunsByStatus(ctx)
	for _, r := range rows {
		st.Runs[r.Status] = int(r.N)
	}
	return st, err
}
