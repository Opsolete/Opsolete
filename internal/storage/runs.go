package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
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

// activeRun is the status list of runs_one_active_idx, at most one per workspace: ON CONFLICT
// clauses on that index must match it exactly.
const activeRun = `('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')`

// working: a runner holds the run's lease.
const working = `status IN ('planning', 'applying')`

// Run is one plan → (confirm) → apply of a workspace, or of its destruction.
type Run struct {
	ID          string
	WorkspaceID string
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
	UpdatedAt       time.Time
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

// queueRuns inserts runs in plan_queued, each with its first history entry. sel is a SELECT of
// (workspace_id, action, created_by, vcs_commit, vcs_pull_request, message), at most one row per
// workspace; a workspace with an active run gets none (runs_one_active_idx). It returns the new runs.
func queueRuns(ctx context.Context, tx *sql.Tx, sel string, args ...any) ([]Run, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH s (workspace_id, action, created_by, vcs_commit, vcs_pull_request, message) AS (`+sel+`),
		r AS (
			INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request)
			SELECT workspace_id, action, 'plan_queued', created_by, vcs_commit, vcs_pull_request FROM s
			ON CONFLICT (workspace_id) WHERE status IN `+activeRun+` DO NOTHING
			RETURNING id, workspace_id, action, created_at
		), h AS (
			INSERT INTO run_status_history (run_id, status, message)
			SELECT r.id, 'plan_queued', s.message FROM r JOIN s USING (workspace_id)
		)
		SELECT id, workspace_id, action, created_at FROM r`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r := Run{Status: PlanQueued}
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Action, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// setStatus moves run runID to status to, with message ("" none) in its history. It is the one
// place a run's status changes: it locks the run, refuses a transition the state machine doesn't
// allow (ErrTransition; sql.ErrNoRows for no such run), and performs what the new status implies:
//   - leaving planning or applying: the lease ends, and with it the state lock the run's Terraform
//     may have left behind (never a rollback's or CLI's)
//   - ending: config_changes is dropped; ended before its apply started (discarded, canceled,
//     errored), the configuration the run changed goes back (see restoreConfig); a destroy run
//     removing a template that succeeded removes it
//
// Status-specific columns (plan, outputs, confirmed_by, runner and lease) stay with the callers.
// It returns the status the run came from.
func setStatus(ctx context.Context, tx *sql.Tx, runID, to, message string) (string, error) {
	var from, workspaceID string
	var removeTemplate sql.NullString
	var changes []byte
	if err := tx.QueryRowContext(ctx, `SELECT status, workspace_id, remove_template, config_changes FROM runs
		WHERE id = ops_uuid($1) FOR UPDATE`, runID).Scan(&from, &workspaceID, &removeTemplate, &changes); err != nil {
		return "", err
	}
	if !canTransition(from, to) {
		return from, fmt.Errorf("%w: run %s from %s to %s", ErrTransition, runID, from, to)
	}
	leased, ended := to == Planning || to == Applying, Ended(to)
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = $2, updated_at = now(),
			lease_id = CASE WHEN $3 THEN lease_id END, lease_until = CASE WHEN $3 THEN lease_until END,
			config_changes = CASE WHEN $4 THEN NULL ELSE config_changes END
		WHERE id = $1::uuid`, runID, to, leased, ended); err != nil {
		return from, err
	}
	if err := addHistory(ctx, tx, runID, to, message); err != nil {
		return from, err
	}
	if !leased {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL
			WHERE id = $1::uuid AND state_lock->>'OpsoleteRun' = $2`, workspaceID, runID); err != nil {
			return from, err
		}
	}
	if (to == Discarded || to == Canceled || to == Errored) && from != Applying && changes != nil {
		if err := restoreConfig(ctx, tx, workspaceID, changes); err != nil {
			return from, err
		}
	}
	if removeTemplate.Valid && (to == Applied || to == NoChanges) { // its resources are gone now
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspace_templates WHERE workspace_id = $1::uuid AND name = $2`,
			workspaceID, removeTemplate.String); err != nil {
			return from, err
		}
	}
	return from, nil
}

// addHistory records an entry in a run's status history: a transition (setStatus), or a note on
// the current status (a cancel request).
func addHistory(ctx context.Context, tx *sql.Tx, runID, status, message string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO run_status_history (run_id, status, message) VALUES ($1::uuid, $2, NULLIF($3, ''))`,
		runID, status, message)
	return err
}

// configChange is one added template's configuration before and after a run's change (configuring
// templates, rollouts). A run that ends before its apply started puts the before back, so the
// workspace never claims a version that wasn't applied; once its apply started the state may already
// hold part of the new one, so it stays.
type configChange struct {
	Module string          `json:"module"`
	Before json.RawMessage `json:"before"` // nil (null): an added template that wasn't there
	After  json.RawMessage `json:"after"`
}

// templateConfig is an added template's configuration (workspace_templates row t) as config_changes
// records it.
const templateConfig = `jsonb_build_object('template_id', t.template_id, 'module_source', t.module_source,
	'module_ref', t.module_ref, 'inputs', t.inputs)`

// configSnapshot reads one added template's configuration in tx; nil if there is none.
func configSnapshot(ctx context.Context, tx *sql.Tx, workspaceID, module string) (json.RawMessage, error) {
	var b []byte
	err := tx.QueryRowContext(ctx, `SELECT `+templateConfig+` FROM workspace_templates t
		WHERE t.workspace_id = ops_uuid($1) AND t.name = $2`, workspaceID, module).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// recordConfigChanges stores on run runID the changes tx made to the templates in changes (Before
// read before the change), reading each After now.
func recordConfigChanges(ctx context.Context, tx *sql.Tx, runID, workspaceID string, changes []configChange) error {
	for i := range changes {
		after, err := configSnapshot(ctx, tx, workspaceID, changes[i].Module)
		if err != nil {
			return err
		}
		changes[i].After = after
	}
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET config_changes = $2 WHERE id = ops_uuid($1)`, runID, b)
	return err
}

// restoreConfig puts back the before of each change (raw config_changes) whose template is still
// at its after: a template changed since is left alone. A before of null removes the template.
func restoreConfig(ctx context.Context, tx *sql.Tx, workspaceID string, raw []byte) error {
	var changes []configChange
	if err := json.Unmarshal(raw, &changes); err != nil {
		return err
	}
	for _, c := range changes {
		if c.After == nil || string(c.After) == "null" {
			continue // nothing to compare with: never restored
		}
		q := `UPDATE workspace_templates t SET template_id = $4::jsonb->>'template_id', module_source = $4::jsonb->>'module_source',
			module_ref = $4::jsonb->>'module_ref', inputs = $4::jsonb->'inputs'`
		args := []any{workspaceID, c.Module, string(c.After), string(c.Before)}
		if c.Before == nil || string(c.Before) == "null" {
			q, args = `DELETE FROM workspace_templates t`, args[:3]
		}
		if _, err := tx.ExecContext(ctx, q+` WHERE t.workspace_id = $1::uuid AND t.name = $2 AND `+templateConfig+` = $3::jsonb`,
			args...); err != nil {
			return err
		}
	}
	return nil
}
