package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
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
//   - ending: config_changes is dropped; ended before its apply started (discarded, canceled,
//     errored), the configuration the run changed goes back (see restoreConfig); a destroy run
//     removing templates that succeeded (applied, or no changes: nothing was left) removes them
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
	if (to == Discarded || to == Canceled || to == Errored) && from != Applying && run.ConfigChanges != nil {
		if err := restoreConfig(ctx, q, run.WorkspaceID, run.ConfigChanges); err != nil {
			return from, nil, err
		}
	}
	succeeded := to == Applied || to == NoChanges
	if succeeded { // the removed templates' resources are gone now
		for _, name := range run.RemoveTemplates {
			if err := q.DeleteWorkspaceTemplate(ctx, db.DeleteWorkspaceTemplateParams{WorkspaceID: run.WorkspaceID, Name: name}); err != nil {
				return from, nil, err
			}
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

// configChange is one added template's configuration before and after a run's change (configuring
// templates, rollouts, promotions). A run that ends before its apply started puts the before back, so the
// workspace never claims a version that wasn't applied; once its apply started the state may already
// hold part of the new one, so it stays. Before and After are template_config (001_schema.sql).
type configChange struct {
	Module string          `json:"module"`
	Before json.RawMessage `json:"before"` // nil (null): an added template that wasn't there
	After  json.RawMessage `json:"after"`  // nil (null): a removed template (a promotion)
}

// configSnapshot reads one added template's configuration; nil if there is none.
func configSnapshot(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, module string) (json.RawMessage, error) {
	b, err := q.ConfigSnapshot(ctx, db.ConfigSnapshotParams{WorkspaceID: workspaceID, Name: module})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// recordConfigChanges stores on run runID the changes made to the templates in changes (Before
// read before the change), reading each After now.
func recordConfigChanges(ctx context.Context, q *db.Queries, runID, workspaceID uuid.UUID, changes []configChange) error {
	for i := range changes {
		after, err := configSnapshot(ctx, q, workspaceID, changes[i].Module)
		if err != nil {
			return err
		}
		changes[i].After = after
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	return q.SetConfigChanges(ctx, db.SetConfigChangesParams{ID: runID, ConfigChanges: b})
}

// restoreConfig puts back the before of each change (raw config_changes):
//   - changed (before and after): only while the template is still at its after; one changed since is left alone
//   - added (before null): removed, only while it is still at its after
//   - removed (after null, a promotion): put back, unless a template took its module name since
func restoreConfig(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, raw []byte) error {
	var changes []configChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return err
	}
	isNull := func(b json.RawMessage) bool { return b == nil || string(b) == "null" }
	for _, c := range changes {
		var err error
		switch {
		case isNull(c.After) && isNull(c.Before):
			continue // nothing was there before or after
		case isNull(c.After):
			err = q.RestoreRemovedTemplate(ctx, db.RestoreRemovedTemplateParams{WorkspaceID: workspaceID, Name: c.Module, Before: c.Before})
		case isNull(c.Before):
			err = q.RemoveRestoredTemplate(ctx, db.RemoveRestoredTemplateParams{WorkspaceID: workspaceID, Name: c.Module, After: c.After})
		default:
			err = q.RestoreTemplate(ctx, db.RestoreTemplateParams{WorkspaceID: workspaceID, Name: c.Module, After: c.After, Before: c.Before})
		}
		if err != nil {
			return err
		}
	}
	return nil
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

// Found is what a plan that imports found for one of its template modules (runnerapi.FinishRequest.Found).
type Found struct {
	TemplateID string         `json:"template_id"`
	Inputs     map[string]any `json:"inputs"`
}

// RunFound is what run runID's plan found importing, by template module; nil none. ErrNotFound: no such run.
func (s *Store) RunFound(ctx context.Context, runID uuid.UUID) (map[string]Found, error) {
	raw, err := s.q.RunFound(ctx, runID)
	if err != nil {
		return nil, notFound(err)
	}
	var found map[string]Found
	if raw != nil {
		err = json.Unmarshal(raw, &found)
	}
	return found, err
}
