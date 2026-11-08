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
var transitions = map[string][]string{
	PlanQueued:        {Planning, Canceled},
	Planning:          {NeedsConfirmation, ApplyQueued, NoChanges, Drifted, Planned, Errored, Canceled},
	NeedsConfirmation: {ApplyQueued, Discarded},
	ApplyQueued:       {Applying, Canceled},
	Applying:          {Applied, Errored, Canceled},
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
//     removing a template that succeeded removes it
//
// Status-specific columns (plan, outputs, confirmed_by, runner and lease) stay with the callers.
// It returns the status the run came from.
func setStatus(ctx context.Context, q *db.Queries, runID uuid.UUID, to, message string) (string, error) {
	run, err := q.LockRun(ctx, runID)
	if err != nil {
		return "", notFound(err)
	}
	from := run.Status
	if !canTransition(from, to) {
		return from, fmt.Errorf("%w: run %s from %s to %s", ErrTransition, runID, from, to)
	}
	leased, ended := to == Planning || to == Applying, Ended(to)
	if err := q.UpdateRunStatus(ctx, db.UpdateRunStatusParams{ID: runID, Status: to, Leased: leased, Ended: ended}); err != nil {
		return from, err
	}
	if err := addHistory(ctx, q, runID, to, message); err != nil {
		return from, err
	}
	if !leased {
		if err := q.ReleaseRunStateLock(ctx, db.ReleaseRunStateLockParams{WorkspaceID: run.WorkspaceID, RunID: runID}); err != nil {
			return from, err
		}
	}
	if (to == Discarded || to == Canceled || to == Errored) && from != Applying && run.ConfigChanges != nil {
		if err := restoreConfig(ctx, q, run.WorkspaceID, run.ConfigChanges); err != nil {
			return from, err
		}
	}
	if run.RemoveTemplate != nil && (to == Applied || to == NoChanges) { // its resources are gone now
		if err := q.DeleteWorkspaceTemplate(ctx, db.DeleteWorkspaceTemplateParams{WorkspaceID: run.WorkspaceID, Name: *run.RemoveTemplate}); err != nil {
			return from, err
		}
	}
	return from, nil
}

// addHistory records an entry in a run's status history: a transition (setStatus), or a note on
// the current status (a cancel request).
func addHistory(ctx context.Context, q *db.Queries, runID uuid.UUID, status, message string) error {
	return q.AddRunHistory(ctx, db.AddRunHistoryParams{RunID: runID, Status: status, Message: message})
}

// configChange is one added template's configuration before and after a run's change (configuring
// templates, rollouts). A run that ends before its apply started puts the before back, so the
// workspace never claims a version that wasn't applied; once its apply started the state may already
// hold part of the new one, so it stays. Before and After are template_config (001_schema.sql).
type configChange struct {
	Module string          `json:"module"`
	Before json.RawMessage `json:"before"` // nil (null): an added template that wasn't there
	After  json.RawMessage `json:"after"`
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

// restoreConfig puts back the before of each change (raw config_changes) whose template is still
// at its after: a template changed since is left alone. A before of null removes the template.
func restoreConfig(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, raw []byte) error {
	var changes []configChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return err
	}
	isNull := func(b json.RawMessage) bool { return b == nil || string(b) == "null" }
	for _, c := range changes {
		var err error
		switch {
		case isNull(c.After):
			continue // nothing to compare with: never restored
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
