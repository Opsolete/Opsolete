package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
	"github.com/opsolete/opsolete/internal/templates"
)

// Run statuses. A workspace has at most one active run (enforced by runs_one_active_idx).
const (
	PlanQueued        = "plan_queued"
	Planning          = "planning"
	NeedsConfirmation = "needs_confirmation"
	ApplyQueued       = "apply_queued"
	Applying          = "applying"
	Applied           = "applied"
	NoChanges         = "no_changes"
	Drifted           = "drifted" // final status of an assess run that found drift
	Planned           = "planned" // final status of a speculative plan that found changes
	Discarded         = "discarded"
	Errored           = "errored"
	Canceled          = "canceled"
)

// transitions is the run state machine: every status a run may move to from each status. A status
// without any is terminal: the run has ended. setStatus is the only writer of runs.status.
// Planning → PlanQueued and Applying → ApplyQueued undo a claim its runner never acknowledged:
// only UnclaimJobs uses them. A promotion's destroy run ending applied or no_changes queues the
// promotion's apply run, a new run (setStatus).
var transitions = map[string][]string{
	PlanQueued:        {Planning, Canceled},
	Planning:          {NeedsConfirmation, ApplyQueued, NoChanges, Drifted, Planned, Errored, Canceled, PlanQueued},
	NeedsConfirmation: {ApplyQueued, Discarded},
	ApplyQueued:       {Applying, Canceled},
	Applying:          {Applied, Errored, Canceled, ApplyQueued},
}

// ErrTransition: a status change the state machine doesn't allow. It means a bug, or a race the
// caller lost (the run moved on meanwhile); callers that race legitimately map it to their own error.
var ErrTransition = errors.New("run status transition not allowed")

func canTransition(from, to string) bool { return slices.Contains(transitions[from], to) }

// Ended: the run reached a terminal status, so it will never change again.
func Ended(status string) bool { return len(transitions[status]) == 0 }

// Run is one plan → (confirm) → apply of a workspace, or of its destruction.
type Run struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Action      string // apply | destroy | refresh (refresh-only) | assess (drift check, never applied) | plan (speculative, never applied)
	Status      string
	Replace     []string        // resource addresses an apply run plans to replace
	Plan        *runnerapi.Plan // nil until planned, or when the plan had no changes
	Outputs     map[string]any
	Runner      *string
	CreatedBy   string // emails
	ConfirmedBy string
	// CancelRequested: a user asked to cancel while a runner works on it.
	CancelRequested bool
	VCSCommit       string // commit a VCS workspace's run planned; "" until its plan fetched the configuration
	VCSPullRequest  int    // pull request a speculative plan was made for; 0 for other runs
	CreatedAt       time.Time
	UpdatedAt       time.Time  // when it ended, once it has
	LogRemovedAt    *time.Time // retention deleted its log (PruneRunLogs); nil while it's kept
}

// LogRemoved is what an ended run whose log was deleted shows instead; "" while the log is kept.
func (r Run) LogRemoved() string {
	if r.LogRemovedAt == nil {
		return ""
	}
	return fmt.Sprintf("The log was removed %d days after the run ended (RUN_LOG_RETENTION_DAYS).", int(r.LogRemovedAt.Sub(r.UpdatedAt)/(24*time.Hour)))
}

// Active: the run still holds its workspace (queued, working or waiting for confirmation).
func (r Run) Active() bool { return !Ended(r.Status) }

// Working: a runner is on it or about to be, so its log is still growing.
func (r Run) Working() bool {
	return r.Status == PlanQueued || r.Status == Planning || r.Status == ApplyQueued || r.Status == Applying
}

type StatusEntry struct {
	Status    string
	Message   *string
	CreatedAt time.Time
}

// setStatus moves run runID to status to, with message ("" none) in its history. It is the one
// place a run's status changes: it locks the run, refuses a transition the state machine doesn't
// allow (ErrTransition; ErrNotFound for no such run), and performs what the new status implies:
//   - leaving planning or applying: the lease ends, and with it the state lock the run's Terraform
//     may have left behind (never a rollback's or CLI's)
//   - its apply starting (applying), or no changes: the workspace takes the run's configuration version
//     (configversions.go); a run that ends before that changes no configuration
//   - a destroy run removing templates that succeeded (applied, or no changes: nothing was left) removes
//     them: a version without them
//   - a promotion's first run (a destroy carrying the pending promotion) that succeeded queues the
//     promotion's apply run, after removing its templates (continuePromotion); one that ended any
//     other way drops the promotion, and nothing else changes
//
// Status-specific columns (plan, outputs, confirmed_by, runner and lease) stay with the callers.
// It returns the status the run came from.
func setStatus(ctx context.Context, q *db.Queries, runID uuid.UUID, to, message string) (string, error) {
	from, _, err := transition(ctx, q, runID, to, message)
	return from, err
}

// transition is setStatus, and returns the promotion step it queued (nil: none) for the caller's
// audit entry: only FinishJob ends a run applied or with no changes.
func transition(ctx context.Context, q *db.Queries, runID uuid.UUID, to, message string) (string, *PromotionStep, error) {
	run, err := q.LockRun(ctx, runID)
	if err != nil {
		return "", nil, notFound(err)
	}
	from := run.Status
	if !canTransition(from, to) {
		return from, nil, fmt.Errorf("%w: run %s from %s to %s", ErrTransition, runID, from, to)
	}
	leased, ended := to == Planning || to == Applying, Ended(to)
	if err := q.UpdateRunStatus(ctx, db.UpdateRunStatusParams{ID: runID, Status: to, Leased: leased, Ended: ended}); err != nil {
		return from, nil, err
	}
	if err := addHistory(ctx, q, runID, to, message); err != nil {
		return from, nil, err
	}
	if err := notifyRun(ctx, q, runID); err != nil {
		return from, nil, err
	}
	if to == PlanQueued || to == ApplyQueued {
		if err := notifyJobs(ctx, q); err != nil {
			return from, nil, err
		}
	}
	if !leased {
		n, err := q.ReleaseRunStateLock(ctx, db.ReleaseRunStateLockParams{WorkspaceID: run.WorkspaceID, RunID: runID})
		if err := locksReleased(ctx, q, n, err); err != nil {
			return from, nil, err
		}
	}
	if (to == Applying || to == NoChanges) && run.CvID.Valid {
		if err := setCurrent(ctx, q, run.WorkspaceID, run.CvID.UUID); err != nil {
			return from, nil, err
		}
	}
	succeeded := to == Applied || to == NoChanges
	if succeeded && len(run.RemoveTemplates) > 0 { // the removed templates' resources are gone now
		cur, mods, err := currentConfig(ctx, q, run.WorkspaceID)
		if err != nil {
			return from, nil, err
		}
		cv, err := newVersion(ctx, q, run.WorkspaceID, cur, without(mods, run.RemoveTemplates), SourceRemove, uuid.Nil)
		if err != nil {
			return from, nil, err
		}
		if err := setCurrent(ctx, q, run.WorkspaceID, cv); err != nil {
			return from, nil, err
		}
	}
	switch {
	case run.Promotion == nil || !ended:
	case succeeded:
		step, err := continuePromotion(ctx, q, runID, run.WorkspaceID, to, run.Promotion)
		return from, step, err
	default:
		return from, nil, addHistory(ctx, q, runID, to, "promotion not continued: "+to)
	}
	return from, nil, nil
}

// Notification channels, listened to by app.Notifier. They are sent inside the transaction of the
// change, so Postgres delivers them on commit (never for a rollback); payloads are ids only.
const (
	JobsChannel = "opsolete_jobs" // a job may have become claimable (no payload)
	RunsChannel = "opsolete_runs" // a run's log or status changed (payload: the run id)
)

// notifyJobs: a run was queued (createRun, QueueVCSRun, QueueAssessments, setStatus) or a state
// lock released (locksReleased).
func notifyJobs(ctx context.Context, q *db.Queries) error {
	return q.Notify(ctx, db.NotifyParams{Channel: JobsChannel})
}

// notifyRun: run id's status changed (setStatus) or its log grew (AppendLog).
func notifyRun(ctx context.Context, q *db.Queries, id uuid.UUID) error {
	return q.Notify(ctx, db.NotifyParams{Channel: RunsChannel, Payload: id.String()})
}

// locksReleased takes the result of a state-lock release query: n released locks may make runs of
// their workspaces claimable (ClaimableRun skips locked workspaces).
func locksReleased(ctx context.Context, q *db.Queries, n int64, err error) error {
	if err != nil || n == 0 {
		return err
	}
	return notifyJobs(ctx, q)
}

// addHistory records an entry in a run's status history: a transition (setStatus), or a note on
// the current status (a cancel request).
func addHistory(ctx context.Context, q *db.Queries, runID uuid.UUID, status, message string) error {
	return q.AddRunHistory(ctx, db.AddRunHistoryParams{RunID: runID, Status: status, Message: message})
}

// pruneBatch is how many runs one retention statement handles: short locks on a big first pass.
const pruneBatch = 1000

// PruneRunLogs deletes the logs of runs that ended more than olderThan ago, marking each run's
// log_removed_at in the same statement. It returns how many runs lost their log.
func (s *Store) PruneRunLogs(ctx context.Context, olderThan time.Duration) (int64, error) {
	var logs int64 // runs that had a log; the batch counts every run it marked
	_, err := prune(func(lim int32) (int64, error) {
		had, err := s.q.PruneRunLogs(ctx, db.PruneRunLogsParams{OlderSecs: olderThan.Seconds(), Lim: lim})
		for _, h := range had {
			if h {
				logs++
			}
		}
		return int64(len(had)), err
	}, pruneBatch)
	return logs, err
}

// PruneSpeculativeRuns deletes drift checks and speculative plans that ended more than olderThan
// ago, except the latest ones their readers need (see the query). It returns how many it deleted.
func (s *Store) PruneSpeculativeRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	return prune(func(lim int32) (int64, error) {
		return s.q.PruneSpeculativeRuns(ctx, db.PruneSpeculativeRunsParams{OlderSecs: olderThan.Seconds(), Lim: lim})
	}, pruneBatch)
}

// prune runs batch until one handles fewer than lim rows, and returns the total.
func prune(batch func(lim int32) (int64, error), lim int32) (int64, error) {
	var total int64
	for {
		n, err := batch(lim)
		total += n
		if err != nil || n < int64(lim) {
			return total, err
		}
	}
}

// Found is, for one template module of a run that changed it and ended without applying (or whose plan imports),
// what it was set to and what its plan found importing (runnerapi.FinishRequest.Found): Change settings starts the
// form from it.
type Found struct {
	TemplateID string         `json:"template_id"`
	Inputs     map[string]any `json:"inputs,omitempty"` // found by the plan
	Saved      map[string]any `json:"saved,omitempty"`  // the module's inputs in the run's version, sensitive (sealed) ones left out
}

// unsealed is inputs without the sealed values: the form never shows them.
func unsealed(inputs map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range inputs {
		if !templates.IsSealed(v) {
			out[k] = v
		}
	}
	return out
}

// RunFound is, by template module, what run runID's plan found importing and, once the run ended without its
// apply, the modules it changed (it made the change: the first run of its version); each with its inputs in the
// run's version (Saved). nil none. ErrNotFound: no such run.
func (s *Store) RunFound(ctx context.Context, runID uuid.UUID) (map[string]Found, error) {
	raw, err := s.q.RunFound(ctx, runID)
	if err != nil {
		return nil, notFound(err)
	}
	var found map[string]Found
	if raw != nil {
		if err := json.Unmarshal(raw, &found); err != nil {
			return nil, err
		}
	}
	rc, err := s.q.RunConfig(ctx, runID)
	if err != nil {
		return nil, err
	}
	mods, err := configModules(ctx, s.q, rc.CvID)
	if err != nil {
		return nil, err
	}
	entered := rc.Changed && !rc.Active && !rc.Applied // ended without its apply
	var before []WorkspaceTemplate
	if entered {
		if before, err = configModules(ctx, s.q, rc.ParentID); err != nil {
			return nil, err
		}
	}
	for _, m := range mods {
		f, read := found[m.Name]
		changed := entered && !slices.ContainsFunc(before, func(b WorkspaceTemplate) bool {
			return sameTemplates([]WorkspaceTemplate{b}, []WorkspaceTemplate{m})
		})
		if !read && !changed {
			continue
		}
		if found == nil {
			found = map[string]Found{}
		}
		f.TemplateID, f.Saved = m.TemplateID, unsealed(m.Inputs)
		found[m.Name] = f
	}
	return found, nil
}
