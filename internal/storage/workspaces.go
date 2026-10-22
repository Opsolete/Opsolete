package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rudesome/form2stack/internal/runnerapi"
)

var (
	ErrNameTaken      = errors.New("a workspace with this name already exists")
	ErrRunActive      = errors.New("the workspace already has an active run: confirm or discard it, or wait for it to finish")
	ErrNotConfirmable = errors.New("the run isn't waiting for confirmation")
	ErrNotCancelable  = errors.New("the run can't be canceled: it isn't queued or running")
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
// empty workspace may hold just variables or an uploaded state.
type Workspace struct {
	ID              string
	Name            string
	TemplateID      string // "" if not created from a template
	ModuleSource    string
	ModuleRef       string
	Inputs          map[string]any
	VCSConnectionID string // "" if not linked to a repository
	VCSRepo         string
	VCSBranch       string
	StateKey        string // blob key prefix of its state versions
	AutoApply       bool   // apply plans of apply runs without confirmation
	CreatedBy       string // email; empty for workspaces from before login
	CreatedAt       time.Time
	Latest          *Run // most recent run
}

// HasConfig: there is Terraform configuration to plan, so runs can start.
func (w Workspace) HasConfig() bool { return w.ModuleSource != "" || w.VCSConnectionID != "" }

// Run statuses. A workspace has at most one active run (enforced by runs_one_active_idx).
const (
	PlanQueued        = "plan_queued"
	Planning          = "planning"
	NeedsConfirmation = "needs_confirmation"
	ApplyQueued       = "apply_queued"
	Applying          = "applying"
	Applied           = "applied"
	NoChanges         = "no_changes"
	Discarded         = "discarded"
	Errored           = "errored"
	Canceled          = "canceled"
)

// Run is one plan → (confirm) → apply of a workspace, or of its destruction.
type Run struct {
	ID          string
	WorkspaceID string
	Action      string // apply | destroy
	Status      string
	Plan        *runnerapi.Plan // nil until planned, or when the plan had no changes
	Outputs     map[string]any
	Runner      *string
	CreatedBy   string // emails
	ConfirmedBy string
	// CancelRequested: a user asked to cancel while a runner works on it.
	CancelRequested bool
	VCSCommit       string // commit a VCS workspace's run planned; "" until its plan fetched the configuration
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
	w.inputs, COALESCE(w.vcs_connection_id::text, ''), COALESCE(w.vcs_repo, ''), COALESCE(w.vcs_branch, ''), w.state_key, w.auto_apply, COALESCE((SELECT email FROM users u WHERE u.id = w.created_by), ''), w.created_at`

func scanWorkspace(row interface{ Scan(...any) error }, extra ...any) (Workspace, error) {
	var w Workspace
	var raw []byte
	dest := append([]any{&w.ID, &w.Name, &w.TemplateID, &w.ModuleSource, &w.ModuleRef, &raw,
		&w.VCSConnectionID, &w.VCSRepo, &w.VCSBranch, &w.StateKey,
		&w.AutoApply, &w.CreatedBy, &w.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return w, err
	}
	return w, json.Unmarshal(raw, &w.Inputs)
}

const runCols = `r.id, r.workspace_id, r.action, r.status, r.plan, r.outputs, r.runner,
	COALESCE((SELECT email FROM users u WHERE u.id = r.created_by), ''),
	COALESCE((SELECT email FROM users u WHERE u.id = r.confirmed_by), ''), r.cancel_requested, COALESCE(r.vcs_commit, ''), r.created_at, r.updated_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var plan, outputs []byte
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.Action, &r.Status, &plan, &outputs, &r.Runner,
		&r.CreatedBy, &r.ConfirmedBy, &r.CancelRequested, &r.VCSCommit, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	if plan != nil {
		if err := json.Unmarshal(plan, &r.Plan); err != nil {
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
			'form2stack/' || COALESCE(NULLIF($2, ''), 'workspaces') || '/' || v || '.tfstate', $6, NULLIF($7, '')::uuid FROM id
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

// CreateRun queues a new plan (action apply or destroy) on a workspace without an active run.
func (s *Store) CreateRun(ctx context.Context, workspaceID, action, createdBy string) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	r, err := createRun(ctx, tx, workspaceID, action, createdBy)
	if err != nil {
		return Run{}, err
	}
	return r, tx.Commit()
}

func createRun(ctx context.Context, tx *sql.Tx, workspaceID, action, createdBy string) (Run, error) {
	r := Run{WorkspaceID: workspaceID, Action: action, Status: PlanQueued}
	err := tx.QueryRowContext(ctx, `INSERT INTO runs (workspace_id, action, status, created_by)
		VALUES ($1::uuid, $2, $3, NULLIF($4, '')::uuid) RETURNING id, created_at`,
		workspaceID, action, PlanQueued, createdBy).Scan(&r.ID, &r.CreatedAt)
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
			WHERE workspace_id = w.id ORDER BY created_at DESC LIMIT 1) l ON true
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
func (s *Store) SetWorkspaceVCS(ctx context.Context, workspaceID, connID, repo, branch string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET vcs_connection_id = NULLIF($2, '')::uuid,
		vcs_repo = NULLIF($3, ''), vcs_branch = NULLIF($4, ''), updated_at = now()
		WHERE id::text = $1 AND template_id IS NULL`, workspaceID, connID, repo, branch)
	return err
}

// PinVCSCommit records the commit a run plans, unless one is recorded already, and returns the
// run's commit: the apply phase gets the commit its plan was made from.
func (s *Store) PinVCSCommit(ctx context.Context, runID, commit string) (string, error) {
	err := s.DB.QueryRowContext(ctx, `UPDATE runs SET vcs_commit = COALESCE(vcs_commit, $2)
		WHERE id::text = $1 RETURNING vcs_commit`, runID, commit).Scan(&commit)
	return commit, err
}

// SetAutoApply changes whether apply plans run without confirmation.
func (s *Store) SetAutoApply(ctx context.Context, workspaceID string, on bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET auto_apply = $2, updated_at = now() WHERE id::text = $1`, workspaceID, on)
	return err
}

// Runs lists a workspace's runs, newest first.
func (s *Store) Runs(ctx context.Context, workspaceID string, limit int) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.workspace_id::text = $1
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

// CurrentOutputs returns the outputs of the workspace's last successful apply. destroyed is true
// when the last successful run was a destroy.
func (s *Store) CurrentOutputs(ctx context.Context, workspaceID string) (outputs map[string]any, destroyed bool, err error) {
	var action string
	var raw []byte
	err = s.DB.QueryRowContext(ctx, `SELECT action, outputs FROM runs WHERE workspace_id::text = $1 AND status = $2
		ORDER BY created_at DESC LIMIT 1`, workspaceID, Applied).Scan(&action, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || raw == nil {
		return nil, action == "destroy", err
	}
	return outputs, action == "destroy", json.Unmarshal(raw, &outputs)
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
