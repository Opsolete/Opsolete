package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

var (
	ErrNameTaken      = errors.New("a workspace with this name already exists")
	ErrRunActive      = errors.New("the workspace already has an active run: confirm or discard it, or wait for it to finish")
	ErrNotConfirmable = errors.New("the run isn't waiting for confirmation")
	ErrNotCancelable  = errors.New("the run can't be canceled: it isn't queued or running")
	ErrHasResources   = errors.New("the workspace's state still manages resources: destroy them first")
	ErrLocalExecution = errors.New("the workspace uses local execution: Terraform runs on your machine, so runs can't start here (change it in the workspace's settings)")
)

// Viewer is who asks: admins see everything, others their own workspaces and those granted to
// one of their groups.
type Viewer struct {
	UserID string
	Admin  bool
}

// visible is the SQL condition for "viewer ($1 = user id, $2 = admin) may see workspace w".
const visible = `($2 OR w.created_by = NULLIF($1, '')::uuid OR EXISTS (SELECT 1 FROM workspace_access a
	JOIN users u ON u.id = NULLIF($1, '')::uuid WHERE a.workspace_id = w.id AND a.group_name = ANY(u.groups)))`

// Workspace is a long-lived home for one Terraform state. Its configuration comes from a template
// (TemplateID, ModuleSource, ModuleRef, Inputs), from a VCS branch (VCS*), or from nowhere yet: an
// empty workspace may hold just variables.
type Workspace struct {
	ID                string
	Name              string
	TemplateID        string // "" if not created from a template
	ModuleSource      string
	ModuleRef         string
	Inputs            map[string]any
	VCSConnectionID   string // "" if not linked to a repository
	VCSRepo           string
	VCSBranch         string
	VCSTrigger        bool   // a new commit on the branch queues an apply run (see vcs.TriggerRuns)
	VCSPullRequests   bool   // open pull requests into the branch get speculative plans
	StateKey          string // blob key prefix of its state versions
	Templates         int    // how many templates were added (see WorkspaceTemplates)
	AutoApply         bool   // apply plans of apply runs without confirmation
	DriftDetection    bool   // queue an assess run (refresh-only plan, never applied) daily
	GlobalRemoteState bool   // every workspace's runs may read this state; else only RemoteStateConsumers
	ExecutionMode     string // remote: runs on Opsolete runners; local: the CLI (cloud block) runs Terraform, no runs
	Drifted           bool   // the latest assessment found drift and nothing was applied since
	CreatedBy         string // email; empty for workspaces from before login
	CreatedAt         time.Time
	Latest            *Run // most recent run
}

// HasConfig: there is Terraform configuration to plan, so runs can start.
func (w Workspace) HasConfig() bool {
	return w.ModuleSource != "" || w.VCSConnectionID != "" || w.Templates > 0
}

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
func (r Run) Active() bool {
	switch r.Status {
	case PlanQueued, Planning, NeedsConfirmation, ApplyQueued, Applying:
		return true
	}
	return false
}

// Working: a runner is on it or about to be, so its log is still growing.
func (r Run) Working() bool {
	return r.Status == PlanQueued || r.Status == Planning || r.Status == ApplyQueued || r.Status == Applying
}

type StatusEntry struct {
	Status    string
	Message   *string
	CreatedAt time.Time
}

const workspaceCols = `w.id, w.name, COALESCE(w.template_id, ''), COALESCE(w.module_source, ''), COALESCE(w.module_ref, ''),
	w.inputs, COALESCE(w.vcs_connection_id::text, ''), COALESCE(w.vcs_repo, ''), COALESCE(w.vcs_branch, ''), w.vcs_trigger, w.vcs_pull_requests, w.state_key,
	(SELECT count(*) FROM workspace_templates t WHERE t.workspace_id = w.id), w.auto_apply, w.drift_detection, w.global_remote_state, w.execution_mode,
	COALESCE((SELECT a.status = 'drifted' AND NOT EXISTS (SELECT 1 FROM runs x WHERE x.workspace_id = w.id
		AND x.status = 'applied' AND x.updated_at > a.updated_at) FROM runs a
		WHERE a.workspace_id = w.id AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted')
		ORDER BY a.created_at DESC LIMIT 1), false), COALESCE((SELECT email FROM users u WHERE u.id = w.created_by), ''), w.created_at`

func scanWorkspace(row interface{ Scan(...any) error }, extra ...any) (Workspace, error) {
	var w Workspace
	var raw []byte
	dest := append([]any{&w.ID, &w.Name, &w.TemplateID, &w.ModuleSource, &w.ModuleRef, &raw,
		&w.VCSConnectionID, &w.VCSRepo, &w.VCSBranch, &w.VCSTrigger, &w.VCSPullRequests, &w.StateKey, &w.Templates,
		&w.AutoApply, &w.DriftDetection, &w.GlobalRemoteState, &w.ExecutionMode, &w.Drifted, &w.CreatedBy, &w.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return w, err
	}
	return w, json.Unmarshal(raw, &w.Inputs)
}

const runCols = `r.id, r.workspace_id, r.action, r.status, r.replace, r.plan, r.outputs, r.runner,
	COALESCE((SELECT email FROM users u WHERE u.id = r.created_by), ''),
	COALESCE((SELECT email FROM users u WHERE u.id = r.confirmed_by), ''), r.cancel_requested, COALESCE(r.vcs_commit, ''), COALESCE(r.vcs_pull_request, 0), r.created_at, r.updated_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var replace, plan, outputs []byte
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.Action, &r.Status, &replace, &plan, &outputs, &r.Runner,
		&r.CreatedBy, &r.ConfirmedBy, &r.CancelRequested, &r.VCSCommit, &r.VCSPullRequest, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	if plan != nil {
		if err := json.Unmarshal(plan, &r.Plan); err != nil {
			return r, err
		}
	}
	if replace != nil {
		if err := json.Unmarshal(replace, &r.Replace); err != nil {
			return r, err
		}
	}
	if outputs != nil {
		if err := json.Unmarshal(outputs, &r.Outputs); err != nil {
			return r, err
		}
	}
	return r, nil
}

// CreateWorkspace creates a workspace from a template and queues its first plan. createdBy is a user id, or empty.
func (s *Store) CreateWorkspace(ctx context.Context, name, templateID, moduleSource, moduleRef string,
	inputs map[string]any, autoApply bool, createdBy string) (Workspace, Run, error) {
	return s.createWorkspace(ctx, name, templateID, moduleSource, moduleRef, inputs, autoApply, createdBy, true)
}

// CreateEmptyWorkspace creates a workspace without configuration and without a run.
func (s *Store) CreateEmptyWorkspace(ctx context.Context, name string, autoApply bool, createdBy string) (Workspace, error) {
	w, _, err := s.createWorkspace(ctx, name, "", "", "", map[string]any{}, autoApply, createdBy, false)
	return w, err
}

func (s *Store) createWorkspace(ctx context.Context, name, templateID, moduleSource, moduleRef string,
	inputs map[string]any, autoApply bool, createdBy string, plan bool) (Workspace, Run, error) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return Workspace{}, Run{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, Run{}, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `
		WITH id AS (SELECT gen_random_uuid() AS v)
		INSERT INTO workspaces (id, name, template_id, module_source, module_ref, inputs, state_key, auto_apply, created_by)
		SELECT v, $1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), $5,
			'opsolete/' || COALESCE(NULLIF($2, ''), 'workspaces') || '/' || v || '.tfstate', $6, NULLIF($7, '')::uuid FROM id
		RETURNING id`, name, templateID, moduleSource, moduleRef, raw, autoApply, createdBy).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Workspace{}, Run{}, ErrNameTaken
	}
	if err != nil {
		return Workspace{}, Run{}, err
	}
	var run Run
	if plan {
		if run, err = createRun(ctx, tx, id, "apply", createdBy); err != nil {
			return Workspace{}, Run{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, Run{}, err
	}
	w, err := s.GetWorkspace(ctx, name)
	return w, run, err
}

// CreateRun queues a new plan (action apply, destroy, refresh or assess) on a workspace without an
// active run; replace lists resource addresses an apply run replaces.
func (s *Store) CreateRun(ctx context.Context, workspaceID, action, createdBy string, replace []string) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	r, err := createRun(ctx, tx, workspaceID, action, createdBy)
	if err != nil {
		return Run{}, err
	}
	if len(replace) > 0 {
		b, err := json.Marshal(replace)
		if err != nil {
			return Run{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET replace = $2 WHERE id = $1`, r.ID, b); err != nil {
			return Run{}, err
		}
		r.Replace = replace
	}
	return r, tx.Commit()
}

func createRun(ctx context.Context, tx *sql.Tx, workspaceID, action, createdBy string) (Run, error) {
	r := Run{WorkspaceID: workspaceID, Action: action, Status: PlanQueued}
	err := tx.QueryRowContext(ctx, `INSERT INTO runs (workspace_id, action, status, created_by)
		SELECT id, $2, $3, NULLIF($4, '')::uuid FROM workspaces WHERE id = $1::uuid AND execution_mode = 'remote'
		RETURNING id, created_at`,
		workspaceID, action, PlanQueued, createdBy).Scan(&r.ID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) { // or deleted meanwhile; then the caller's update found nothing first
		return Run{}, ErrLocalExecution
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Run{}, ErrRunActive
	}
	if err != nil {
		return Run{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO run_status_history (run_id, status, message)
		VALUES ($1, $2, $3 || ' plan queued, waiting for a runner')`, r.ID, PlanQueued, action)
	return r, err
}

// GetWorkspace returns sql.ErrNoRows for an unknown name.
func (s *Store) GetWorkspace(ctx context.Context, name string) (Workspace, error) {
	return scanWorkspace(s.DB.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces w WHERE w.name = $1`, name))
}

// GetWorkspaceByID returns sql.ErrNoRows if the id is unknown or not a valid uuid.
func (s *Store) GetWorkspaceByID(ctx context.Context, id string) (Workspace, error) {
	return scanWorkspace(s.DB.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces w WHERE w.id::text = $1`, id))
}

// ListWorkspaces returns the 200 most recently active workspaces v may see, with their latest run.
func (s *Store) ListWorkspaces(ctx context.Context, v Viewer) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workspaceCols+`, l.status, l.action, l.updated_at
		FROM workspaces w LEFT JOIN LATERAL (SELECT status, action, updated_at FROM runs
			WHERE workspace_id = w.id AND action NOT IN ('assess', 'plan') ORDER BY created_at DESC LIMIT 1) l ON true
		WHERE `+visible+`
		ORDER BY COALESCE(l.updated_at, w.created_at) DESC LIMIT 200`, v.UserID, v.Admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var status, action sql.NullString
		var updated sql.NullTime
		w, err := scanWorkspace(rows, &status, &action, &updated)
		if err != nil {
			return nil, err
		}
		if status.Valid {
			w.Latest = &Run{Status: status.String, Action: action.String, UpdatedAt: updated.Time}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RenameWorkspace changes a workspace's name; its state and runs are keyed by id and stay put.
func (s *Store) RenameWorkspace(ctx context.Context, id, name string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET name = $2, updated_at = now() WHERE id::text = $1`, id, name)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrNameTaken
	}
	return err
}

// DeleteWorkspace deletes a workspace with its runs, variables, grants and state versions, and
// returns the blob keys of its state files for the caller to delete. It refuses while a run is
// active (ErrRunActive) or the current state manages resources or isn't indexed (ErrHasResources),
// so deleting never orphans real infrastructure.
func (s *Store) DeleteWorkspace(ctx context.Context, id string) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireIdle(ctx, tx, id); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT blob_key FROM state_versions WHERE workspace_id::text = $1`, id)
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id::text = $1`, id); err != nil {
		return nil, err
	}
	return keys, tx.Commit()
}

// requireIdle locks the workspace row (keeping new runs out until tx ends: their FK check waits)
// and returns ErrRunActive if a run is active, or ErrHasResources if its current state manages
// resources or isn't indexed; sql.ErrNoRows if there is no such workspace.
func requireIdle(ctx context.Context, tx *sql.Tx, id string) error {
	var active, deployed bool
	if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM runs WHERE workspace_id = w.id AND status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')),
			EXISTS (SELECT 1 FROM (SELECT v.id, v.indexed FROM state_versions v WHERE v.workspace_id = w.id ORDER BY v.id DESC LIMIT 1) cur
				WHERE NOT cur.indexed OR EXISTS (SELECT 1 FROM state_version_resources r WHERE r.state_version_id = cur.id))
		FROM workspaces w WHERE w.id::text = $1 FOR UPDATE`, id).Scan(&active, &deployed); err != nil {
		return err
	}
	switch {
	case active:
		return ErrRunActive
	case deployed:
		return ErrHasResources
	}
	return nil
}

// Access returns what userID may do in a workspace through being its creator or through a group
// grant: "operate", "read" or "". Admins are handled by the caller.
func (s *Store) Access(ctx context.Context, workspaceID, userID string) (string, error) {
	var level string
	err := s.DB.QueryRowContext(ctx, `SELECT CASE
			WHEN w.created_by IS NOT NULL AND w.created_by = NULLIF($2, '')::uuid THEN 'operate'
			WHEN bool_or(a.level = 'operate') THEN 'operate'
			WHEN count(a.level) > 0 THEN 'read' ELSE '' END
		FROM workspaces w
		LEFT JOIN users u ON u.id = NULLIF($2, '')::uuid
		LEFT JOIN workspace_access a ON a.workspace_id = w.id AND a.group_name = ANY(u.groups)
		WHERE w.id::text = $1 GROUP BY w.id, w.created_by`, workspaceID, userID).Scan(&level)
	return level, err
}

// Grant is one group's access to a workspace.
type Grant struct {
	Group string
	Level string // read | operate
}

// Grants lists a workspace's group grants.
func (s *Store) Grants(ctx context.Context, workspaceID string) ([]Grant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT group_name, level FROM workspace_access
		WHERE workspace_id::text = $1 ORDER BY group_name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Group, &g.Level); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// KnownGroups lists the IdP groups of everyone who has logged in, for picking a grant.
func (s *Store) KnownGroups(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT g FROM users, unnest(groups) g ORDER BY g`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGrant gives a group read or operate access to a workspace; level "" removes the grant.
func (s *Store) SetGrant(ctx context.Context, workspaceID, group, level string) error {
	if level == "" {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM workspace_access WHERE workspace_id::text = $1 AND group_name = $2`, workspaceID, group)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO workspace_access (workspace_id, group_name, level) VALUES ($1::uuid, $2, $3)
		ON CONFLICT (workspace_id, group_name) DO UPDATE SET level = EXCLUDED.level`, workspaceID, group, level)
	return err
}

// SetWorkspaceVCS links a workspace without a template to a repository branch; connID "" unlinks it.
// trigger and pullRequests set which VCS events queue runs. A changed link, or turning on pull
// request plans, detaches the workspace's variable sets: an admin must attach them again.
func (s *Store) SetWorkspaceVCS(ctx context.Context, workspaceID, connID, repo, branch string, trigger, pullRequests bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var detach bool
	err = tx.QueryRowContext(ctx, `SELECT (vcs_connection_id, vcs_repo, vcs_branch) IS DISTINCT FROM
		(NULLIF($2, '')::uuid, NULLIF($3, ''), NULLIF($4, '')) OR (NOT vcs_pull_requests AND $5)
		FROM workspaces WHERE id::text = $1 AND template_id IS NULL FOR UPDATE`,
		workspaceID, connID, repo, branch, pullRequests && connID != "").Scan(&detach)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted meanwhile, or a template workspace: nothing to link
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET vcs_connection_id = NULLIF($2, '')::uuid,
		vcs_repo = NULLIF($3, ''), vcs_branch = NULLIF($4, ''), vcs_trigger = $5, vcs_pull_requests = $6, updated_at = now()
		WHERE id::text = $1 AND template_id IS NULL`, workspaceID, connID, repo, branch, trigger && connID != "", pullRequests && connID != ""); err != nil {
		return err
	}
	// The admin who attached sets vouched for the old repository and branch, not for the new one,
	// nor for unreviewed pull requests.
	if detach {
		if _, err := tx.ExecContext(ctx, `DELETE FROM variable_set_workspaces WHERE workspace_id::text = $1`, workspaceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VCSTriggered lists the remote workspaces whose branch pushes or pull requests queue runs.
func (s *Store) VCSTriggered(ctx context.Context) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workspaceCols+` FROM workspaces w
		WHERE w.vcs_connection_id IS NOT NULL AND (w.vcs_trigger OR w.vcs_pull_requests) AND w.execution_mode = 'remote'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// QueueVCSRun queues a run of commit, pinned at once: an apply run for a push (pullRequest 0), or a
// speculative plan of a pull request. It queues nothing (false) if the workspace has an active run,
// or a run of this commit exists already: for a push any run but a drift check or speculative plan,
// for a pull request a speculative plan of it. The poller retries at its next round.
func (s *Store) QueueVCSRun(ctx context.Context, workspaceID, commit string, pullRequest int, message string) (bool, error) {
	action := "apply"
	if pullRequest > 0 {
		action = "plan"
	}
	var id string
	err := s.DB.QueryRowContext(ctx, `
		WITH r AS (
			INSERT INTO runs (workspace_id, action, status, vcs_commit, vcs_pull_request)
			SELECT w.id, $2, 'plan_queued', $3, NULLIF($4, 0) FROM workspaces w
			WHERE w.id::text = $1 AND w.execution_mode = 'remote' AND NOT EXISTS (SELECT 1 FROM runs x
				WHERE x.workspace_id = w.id AND x.vcs_commit = $3
					AND CASE WHEN $4 > 0 THEN x.action = 'plan' AND x.vcs_pull_request = $4 ELSE x.action NOT IN ('assess', 'plan') END)
			ON CONFLICT (workspace_id) WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
			DO NOTHING RETURNING id
		)
		INSERT INTO run_status_history (run_id, status, message) SELECT id, 'plan_queued', $5 FROM r RETURNING run_id`,
		workspaceID, action, commit, pullRequest, message).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PinVCSCommit records the commit a run plans, unless one is recorded already, and returns the
// run's commit: the apply phase gets the commit its plan was made from.
func (s *Store) PinVCSCommit(ctx context.Context, runID, commit string) (string, error) {
	err := s.DB.QueryRowContext(ctx, `UPDATE runs SET vcs_commit = COALESCE(vcs_commit, $2)
		WHERE id::text = $1 RETURNING vcs_commit`, runID, commit).Scan(&commit)
	return commit, err
}

// SetSettings changes whether apply plans run without confirmation and whether drift is checked daily.
func (s *Store) SetSettings(ctx context.Context, workspaceID string, autoApply, driftDetection bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET auto_apply = $2, drift_detection = $3, updated_at = now()
		WHERE id::text = $1`, workspaceID, autoApply, driftDetection)
	return err
}

// SetExecutionMode sets a workspace's execution mode, "remote" or "local". Runs and CLI locks
// already under way finish; the mode only refuses new ones.
func (s *Store) SetExecutionMode(ctx context.Context, workspaceID, mode string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET execution_mode = $2, updated_at = now() WHERE id::text = $1`, workspaceID, mode)
	return err
}

// RemoteStateConsumers lists the workspaces explicitly allowed to read a workspace's state, by name.
func (s *Store) RemoteStateConsumers(ctx context.Context, workspaceID string) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.name FROM remote_state_consumers c JOIN workspaces w ON w.id = c.consumer_id
		WHERE c.workspace_id::text = $1 ORDER BY w.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetRemoteStateSharing shares a workspace's state with all workspaces (global) or with the
// consumers (workspace ids; unknown ids and the workspace itself are ignored).
func (s *Store) SetRemoteStateSharing(ctx context.Context, workspaceID string, global bool, consumers []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET global_remote_state = $2, updated_at = now() WHERE id::text = $1`,
		workspaceID, global); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM remote_state_consumers WHERE workspace_id::text = $1`, workspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO remote_state_consumers (workspace_id, consumer_id)
		SELECT $1::uuid, id FROM workspaces WHERE id::text = ANY($2::text[]) AND id <> $1::uuid`, workspaceID, consumers); err != nil {
		return err
	}
	return tx.Commit()
}

// SharesState reports whether consumerID's runs may read workspaceID's state.
func (s *Store) SharesState(ctx context.Context, workspaceID, consumerID string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces w WHERE w.id::text = $1 AND w.id::text <> $2
		AND (w.global_remote_state OR EXISTS (SELECT 1 FROM remote_state_consumers c WHERE c.workspace_id = w.id AND c.consumer_id::text = $2)))`,
		workspaceID, consumerID).Scan(&ok)
	return ok, err
}

// QueueAssessments queues an assess run (a refresh-only plan that is never applied) on every
// workspace with drift detection whose current state manages resources and that wasn't assessed
// within every. Workspaces with an active run are skipped; they get their turn at a later call.
func (s *Store) QueueAssessments(ctx context.Context, every time.Duration) (int, error) {
	res, err := s.DB.ExecContext(ctx, `
		WITH due AS (
			SELECT w.id FROM workspaces w
			WHERE w.drift_detection AND w.execution_mode = 'remote'
				AND (w.module_source IS NOT NULL OR w.vcs_connection_id IS NOT NULL
					OR EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id))
				AND EXISTS (SELECT 1 FROM state_version_resources r WHERE r.state_version_id =
					(SELECT max(v.id) FROM state_versions v WHERE v.workspace_id = w.id))
				AND NOT EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = w.id AND a.action = 'assess'
					AND a.created_at > now() - make_interval(secs => $1))
		), r AS (
			INSERT INTO runs (workspace_id, action, status) SELECT id, 'assess', 'plan_queued' FROM due
			ON CONFLICT (workspace_id) WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
			DO NOTHING RETURNING id
		)
		INSERT INTO run_status_history (run_id, status, message)
		SELECT id, 'plan_queued', 'drift detection: refresh-only plan queued, waiting for a runner' FROM r`, every.Seconds())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Runs lists a workspace's runs, newest first; assessments (drift checks) are not listed.
func (s *Store) Runs(ctx context.Context, workspaceID string, limit int) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.workspace_id::text = $1 AND r.action <> 'assess'
		ORDER BY r.created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunMetrics summarizes a workspace's runs (drift checks excluded) for its overview page.
type RunMetrics struct {
	Runs, Failed      int
	AvgPlan, AvgApply time.Duration // 0 when no plan or apply has finished
}

// RunMetrics: a phase lasts from its planning/applying entry to the next history entry.
func (s *Store) RunMetrics(ctx context.Context, workspaceID string) (RunMetrics, error) {
	var m RunMetrics
	var plan, apply float64
	err := s.DB.QueryRowContext(ctx, `WITH r AS (SELECT id, status FROM runs WHERE workspace_id::text = $1 AND action <> 'assess'),
		h AS (SELECT h.status, lead(h.created_at) OVER (PARTITION BY h.run_id ORDER BY h.id) - h.created_at AS d
			FROM run_status_history h JOIN r ON r.id = h.run_id)
		SELECT (SELECT count(*) FROM r), (SELECT count(*) FROM r WHERE status = 'errored'),
			COALESCE(EXTRACT(EPOCH FROM (SELECT avg(d) FROM h WHERE status = 'planning')), 0)::float8,
			COALESCE(EXTRACT(EPOCH FROM (SELECT avg(d) FROM h WHERE status = 'applying')), 0)::float8`,
		workspaceID).Scan(&m.Runs, &m.Failed, &plan, &apply)
	m.AvgPlan, m.AvgApply = time.Duration(plan*float64(time.Second)), time.Duration(apply*float64(time.Second))
	return m, err
}

// LatestAssessment returns the workspace's most recent assess run; sql.ErrNoRows if there is none.
func (s *Store) LatestAssessment(ctx context.Context, workspaceID string) (Run, error) {
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.workspace_id::text = $1
		AND r.action = 'assess' ORDER BY r.created_at DESC LIMIT 1`, workspaceID))
}

// CurrentOutputs returns the outputs of the workspace's current state version. destroyed is true
// when the last successful run was a destroy.
func (s *Store) CurrentOutputs(ctx context.Context, workspaceID string) (outputs map[string]any, destroyed bool, err error) {
	var raw []byte
	err = s.DB.QueryRowContext(ctx, `SELECT
		(SELECT outputs FROM state_versions WHERE workspace_id::text = $1 ORDER BY id DESC LIMIT 1),
		COALESCE((SELECT action = 'destroy' FROM runs WHERE workspace_id::text = $1 AND status = $2
			ORDER BY created_at DESC LIMIT 1), false)`, workspaceID, Applied).Scan(&raw, &destroyed)
	if err != nil || raw == nil {
		return nil, destroyed, err
	}
	return outputs, destroyed, json.Unmarshal(raw, &outputs)
}

// GetRun returns a run and its status history; sql.ErrNoRows if the id is unknown or not a uuid.
func (s *Store) GetRun(ctx context.Context, id string) (Run, []StatusEntry, error) {
	r, err := scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.id::text = $1`, id))
	if err != nil {
		return r, nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT status, message, created_at FROM run_status_history
		WHERE run_id = $1 ORDER BY id`, r.ID)
	if err != nil {
		return r, nil, err
	}
	defer rows.Close()
	var hist []StatusEntry
	for rows.Next() {
		var e StatusEntry
		if err := rows.Scan(&e.Status, &e.Message, &e.CreatedAt); err != nil {
			return r, nil, err
		}
		hist = append(hist, e)
	}
	return r, hist, rows.Err()
}

// ConfirmRun queues the apply of a plan that waits for confirmation.
func (s *Store) ConfirmRun(ctx context.Context, id, userID, email string) error {
	return s.decide(ctx, id, ApplyQueued, userID, "confirmed by "+email+", waiting for a runner")
}

// DiscardRun ends a run whose plan waits for confirmation, without applying it.
func (s *Store) DiscardRun(ctx context.Context, id, email string) error {
	return s.decide(ctx, id, Discarded, "", "discarded by "+email)
}

// decide moves a run out of needs_confirmation; confirmedBy (a user id) is recorded when set.
func (s *Store) decide(ctx context.Context, id, status, confirmedBy, message string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = $2, confirmed_by = COALESCE(NULLIF($3, '')::uuid, confirmed_by),
		updated_at = now() WHERE id::text = $1 AND status = 'needs_confirmation'`, id, status, confirmedBy)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotConfirmable
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_status_history (run_id, status, message) VALUES ($1::uuid, $2, $3)`,
		id, status, message); err != nil {
		return err
	}
	return tx.Commit()
}
