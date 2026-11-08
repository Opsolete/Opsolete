package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrNameTaken      = errors.New("a workspace with this name already exists")
	ErrRunActive      = errors.New("the workspace already has an active run: confirm or discard it, or wait for it to finish")
	ErrNotConfirmable = errors.New("the run isn't waiting for confirmation")
	ErrNotCancelable  = errors.New("the run can't be canceled: it isn't queued or running")
	ErrHasResources   = errors.New("the workspace's state still manages resources: destroy them first")
	ErrStateLocked    = errors.New("the workspace's state is locked: wait for whoever holds the lock, or unlock it")
	ErrLocalExecution = errors.New("the workspace uses local execution: Terraform runs on your machine, so runs can't start here (change it in the workspace's settings)")
)

// Viewer is who asks: admins see everything, others their own workspaces and those granted to
// one of their groups, directly or through the workspace's project.
type Viewer struct {
	UserID string
	Admin  bool
}

// visible is the SQL condition for "viewer ($1 = user id, $2 = admin) may see workspace w".
const visible = `($2 OR access_level(w.id, NULLIF($1, '')::uuid) IS NOT NULL)`

// Workspace is a long-lived home for one Terraform state. Its configuration is the templates added
// to it (WorkspaceTemplates), next to the root configuration of a VCS branch (VCS*) or none: an
// empty workspace may hold just variables.
type Workspace struct {
	ID                string
	Name              string
	VCSConnectionID   string // "" if not linked to a repository
	VCSRepo           string
	VCSBranch         string
	VCSTrigger        bool   // a new commit on the branch queues an apply run (see vcs.TriggerRuns)
	VCSPullRequests   bool   // open pull requests into the branch get speculative plans
	VCSTagsRegex      string // tag-based: new tags matching it queue runs instead of branch commits; "" branch-based
	VCSTagBaseline    string // commit of the newest matching tag when tag triggers were set: never queued
	Templates         int    // how many templates were added (see WorkspaceTemplates)
	AutoApply         bool   // apply plans of apply runs without confirmation
	DriftDetection    bool   // queue an assess run (refresh-only plan, never applied) daily
	GlobalRemoteState bool   // every workspace's runs may read this state; else only RemoteStateConsumers
	ExecutionMode     string // remote: runs on Opsolete runners; local: the CLI (cloud block) runs Terraform, no runs
	Drifted           bool   // the latest assessment found drift and nothing was applied since
	CreatedBy         string // email; empty for workspaces from before login
	CreatedAt         time.Time
	ProjectID         string
	ProjectName       string
	Latest            *Run // most recent run
}

// HasConfig: there is Terraform configuration to plan, so runs can start.
func (w Workspace) HasConfig() bool {
	return w.VCSConnectionID != "" || w.Templates > 0
}

// workspaceCols are scanWorkspace's columns of workspaceFrom.
const workspaceCols = `w.id, w.name, COALESCE(vcs.connection_id::text, ''), COALESCE(vcs.repo, ''), COALESCE(vcs.branch, ''),
	COALESCE(vcs.trigger, false), COALESCE(vcs.pull_requests, false), COALESCE(vcs.tags_regex, ''), COALESCE(vcs.tag_baseline, ''),
	(SELECT count(*) FROM workspace_templates t WHERE t.workspace_id = w.id), w.auto_apply, w.drift_detection, w.global_remote_state, w.execution_mode,
	COALESCE((SELECT a.status = 'drifted' AND NOT EXISTS (SELECT 1 FROM runs x WHERE x.workspace_id = w.id
		AND x.status = 'applied' AND x.updated_at > a.updated_at) FROM runs a
		WHERE a.workspace_id = w.id AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted')
		ORDER BY a.created_at DESC LIMIT 1), false), COALESCE((SELECT email FROM users u WHERE u.id = w.created_by), ''), w.created_at,
	w.project_id, (SELECT p.name FROM projects p WHERE p.id = w.project_id)`

const workspaceFrom = `workspaces w LEFT JOIN workspace_vcs vcs ON vcs.workspace_id = w.id`

func scanWorkspace(row interface{ Scan(...any) error }, extra ...any) (Workspace, error) {
	var w Workspace
	dest := append([]any{&w.ID, &w.Name, &w.VCSConnectionID, &w.VCSRepo, &w.VCSBranch, &w.VCSTrigger, &w.VCSPullRequests, &w.VCSTagsRegex, &w.VCSTagBaseline, &w.Templates,
		&w.AutoApply, &w.DriftDetection, &w.GlobalRemoteState, &w.ExecutionMode, &w.Drifted, &w.CreatedBy, &w.CreatedAt, &w.ProjectID, &w.ProjectName}, extra...)
	err := row.Scan(dest...)
	return w, err
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

// CreateWorkspace creates a workspace with template t added to it and queues its first plan. createdBy
// is a user id, or empty. id is the new workspace's id ("" = a random one): set when inputs are sealed
// to it beforehand. project is the name of the project it goes into; "" is the default project.
func (s *Store) CreateWorkspace(ctx context.Context, id, name, project string, t WorkspaceTemplate, autoApply bool, createdBy string) (Workspace, Run, error) {
	return s.createWorkspace(ctx, id, name, project, &t, autoApply, createdBy)
}

// CreateEmptyWorkspace creates a workspace without configuration and without a run.
func (s *Store) CreateEmptyWorkspace(ctx context.Context, name, project string, autoApply bool, createdBy string) (Workspace, error) {
	w, _, err := s.createWorkspace(ctx, "", name, project, nil, autoApply, createdBy)
	return w, err
}

func (s *Store) createWorkspace(ctx context.Context, id, name, project string, t *WorkspaceTemplate, autoApply bool, createdBy string) (Workspace, Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, Run{}, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `
		WITH id AS (SELECT COALESCE(NULLIF($4, '')::uuid, gen_random_uuid()) AS v)
		INSERT INTO workspaces (id, name, auto_apply, created_by, project_id)
		SELECT v, $1, $2, NULLIF($3, '')::uuid,
			-- an unknown project name is NULL: the insert fails instead of silently using the default
			CASE WHEN $5 = '' THEN o.default_project_id ELSE (SELECT p.id FROM projects p WHERE p.name = $5) END
		FROM id, organization o
		RETURNING id`, name, autoApply, createdBy, id, project).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Workspace{}, Run{}, ErrNameTaken
	}
	if err != nil {
		return Workspace{}, Run{}, err
	}
	var run Run
	if t != nil {
		raw, err := json.Marshal(t.Inputs)
		if err != nil {
			return Workspace{}, Run{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)`, id, t.Name, t.TemplateID, t.ModuleSource, t.ModuleRef, raw); err != nil {
			return Workspace{}, Run{}, err
		}
		if run, err = createRun(ctx, tx, id, "apply", createdBy); err != nil {
			return Workspace{}, Run{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, Run{}, err
	}
	w, err := s.GetWorkspaceByID(ctx, id)
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

// createRun queues a run on a remote workspace: ErrRunActive if it has an active run,
// ErrLocalExecution if it uses local execution (or was deleted meanwhile).
func createRun(ctx context.Context, tx *sql.Tx, workspaceID, action, createdBy string) (Run, error) {
	runs, err := queueRuns(ctx, tx, `SELECT id, $2::text, NULLIF($3, '')::uuid, NULL::text, NULL::integer,
		$2::text || ' plan queued, waiting for a runner' FROM workspaces WHERE id = $1::uuid AND execution_mode = 'remote'`,
		workspaceID, action, createdBy)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		var remote bool
		if err := tx.QueryRowContext(ctx, `SELECT execution_mode = 'remote' FROM workspaces WHERE id = $1::uuid`, workspaceID).Scan(&remote); err == nil && remote {
			return Run{}, ErrRunActive
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Run{}, err
		}
		return Run{}, ErrLocalExecution
	}
	return runs[0], nil
}

// GetWorkspace returns sql.ErrNoRows for an unknown name.
func (s *Store) GetWorkspace(ctx context.Context, name string) (Workspace, error) {
	return scanWorkspace(s.DB.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM `+workspaceFrom+` WHERE w.name = $1`, name))
}

// GetWorkspaceByID returns sql.ErrNoRows if the id is unknown or not a valid uuid.
func (s *Store) GetWorkspaceByID(ctx context.Context, id string) (Workspace, error) {
	return scanWorkspace(s.DB.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM `+workspaceFrom+` WHERE w.id = ops_uuid($1)`, id))
}

// ListWorkspaces returns the 200 most recently active workspaces v may see, with their latest run;
// project (a name) limits them to one project, "" lists all.
func (s *Store) ListWorkspaces(ctx context.Context, v Viewer, project string) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workspaceCols+`, l.status, l.action, l.updated_at
		FROM `+workspaceFrom+` LEFT JOIN LATERAL (SELECT status, action, updated_at FROM runs
			WHERE workspace_id = w.id AND action NOT IN ('assess', 'plan') ORDER BY created_at DESC LIMIT 1) l ON true
		WHERE `+visible+` AND ($3 = '' OR w.project_id = (SELECT p.id FROM projects p WHERE p.name = $3))
		ORDER BY COALESCE(l.updated_at, w.created_at) DESC LIMIT 200`, v.UserID, v.Admin, project)
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

// WorkspacesExist reports whether there is a workspace for each name.
func (s *Store) WorkspacesExist(ctx context.Context, names []string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM unnest($1::text[]) n
		WHERE NOT EXISTS (SELECT 1 FROM workspaces WHERE name = n))`, names).Scan(&ok)
	return ok, err
}

// DeleteWorkspace deletes a workspace with its runs, variables, grants and state versions, and
// returns the blob keys of its state files for the caller to delete. It refuses while a run is
// active (ErrRunActive) or the current state manages resources or isn't indexed (ErrHasResources),
// so deleting never orphans real infrastructure. force skips the resources check (the state may be
// lost or the resources gone outside Terraform); an active run still refuses.
func (s *Store) DeleteWorkspace(ctx context.Context, id string, force bool) ([]string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireIdle(ctx, tx, id); err != nil && !(force && errors.Is(err, ErrHasResources)) {
		return nil, err
	}
	// even with force: a CLI's first apply holds the lock but has no state version yet
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT state_lock IS NOT NULL FROM workspaces WHERE id = ops_uuid($1)`, id).Scan(&locked); err != nil {
		return nil, err
	}
	if locked {
		return nil, ErrStateLocked
	}
	rows, err := tx.QueryContext(ctx, `SELECT blob_key FROM state_versions WHERE workspace_id = ops_uuid($1)`, id)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ops_uuid($1)`, id); err != nil {
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
			EXISTS (SELECT 1 FROM runs WHERE workspace_id = w.id AND status IN `+activeRun+`),
			EXISTS (SELECT 1 FROM (SELECT v.id, v.indexed FROM state_versions v WHERE v.workspace_id = w.id ORDER BY v.id DESC LIMIT 1) cur
				WHERE NOT cur.indexed OR EXISTS (SELECT 1 FROM state_version_resources r WHERE r.state_version_id = cur.id))
		FROM workspaces w WHERE w.id = ops_uuid($1) FOR UPDATE`, id).Scan(&active, &deployed); err != nil {
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

// Access returns what userID may do in a workspace (access_level): "operate", "read" or "". Admins
// are handled by the caller.
func (s *Store) Access(ctx context.Context, workspaceID, userID string) (string, error) {
	var level string
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(access_level(ops_uuid($1), NULLIF($2, '')::uuid), '')`,
		workspaceID, userID).Scan(&level)
	return level, err
}

// Grant is one group's access to a workspace or project.
type Grant struct {
	Group string
	Level string // read | operate
}

// Grants lists a workspace's group grants.
func (s *Store) Grants(ctx context.Context, workspaceID string) ([]Grant, error) {
	return s.grants(ctx, "workspace_access", "workspace_id", workspaceID)
}

// ProjectGrants lists a project's group grants; they reach all of its workspaces.
func (s *Store) ProjectGrants(ctx context.Context, projectID string) ([]Grant, error) {
	return s.grants(ctx, "project_access", "project_id", projectID)
}

// grants lists the grants of table (workspace_access or project_access) whose col is id.
func (s *Store) grants(ctx context.Context, table, col, id string) ([]Grant, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT group_name, level FROM `+table+`
		WHERE `+col+` = ops_uuid($1) ORDER BY group_name`, id)
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
	return s.setGrant(ctx, "workspace_access", "workspace_id", workspaceID, group, level)
}

// SetProjectGrant gives a group read or operate access to all of a project's workspaces; level "" removes it.
func (s *Store) SetProjectGrant(ctx context.Context, projectID, group, level string) error {
	return s.setGrant(ctx, "project_access", "project_id", projectID, group, level)
}

// setGrant: table and col are workspace_access/workspace_id or project_access/project_id, never input.
func (s *Store) setGrant(ctx context.Context, table, col, id, group, level string) error {
	if level == "" {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+col+` = ops_uuid($1) AND group_name = $2`, id, group)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO `+table+` (`+col+`, group_name, level) VALUES ($1::uuid, $2, $3)
		ON CONFLICT (`+col+`, group_name) DO UPDATE SET level = EXCLUDED.level`, id, group, level)
	return err
}

// SetWorkspaceVCS links a workspace to a repository branch, its root configuration; connID "" unlinks it.
// trigger, pullRequests and tagsRegex (tag-based triggers) set which VCS events queue runs; with a
// changed connection, repository or tag format, tagBaseline becomes the tag commit never to queue.
// A changed link or tag format, or turning on pull request plans, detaches the workspace's variable
// sets: an admin must attach them again. It returns ErrRunActive while a run is active: one queued
// now would run another repository's commit.
func (s *Store) SetWorkspaceVCS(ctx context.Context, workspaceID, connID, repo, branch, tagsRegex, tagBaseline string, trigger, pullRequests bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var detach, active bool
	// FOR UPDATE: the poller's run inserts (their foreign key check) wait until this commits
	err = tx.QueryRowContext(ctx, `SELECT (vcs.connection_id, vcs.repo, vcs.branch, COALESCE(vcs.tags_regex, '')) IS DISTINCT FROM
		(NULLIF($2, '')::uuid, NULLIF($3, ''), NULLIF($4, ''), $6) OR (NOT COALESCE(vcs.pull_requests, false) AND $5),
		EXISTS (SELECT 1 FROM runs WHERE workspace_id = w.id AND status IN `+activeRun+`)
		FROM `+workspaceFrom+` WHERE w.id = ops_uuid($1) FOR UPDATE OF w`,
		workspaceID, connID, repo, branch, pullRequests && connID != "", tagsRegex).Scan(&detach, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted meanwhile, or a template workspace: nothing to link
	}
	if err != nil {
		return err
	}
	if active {
		return ErrRunActive
	}
	if connID == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM workspace_vcs WHERE workspace_id = ops_uuid($1)`, workspaceID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO workspace_vcs AS v (workspace_id, connection_id, repo, branch, trigger, pull_requests, tags_regex, tag_baseline)
			VALUES (ops_uuid($1), $2::uuid, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (workspace_id) DO UPDATE SET connection_id = EXCLUDED.connection_id, repo = EXCLUDED.repo, branch = EXCLUDED.branch,
				trigger = EXCLUDED.trigger, pull_requests = EXCLUDED.pull_requests, tags_regex = EXCLUDED.tags_regex,
				tag_baseline = CASE WHEN (v.connection_id, v.repo, v.tags_regex) IS DISTINCT FROM (EXCLUDED.connection_id, EXCLUDED.repo, EXCLUDED.tags_regex)
					THEN EXCLUDED.tag_baseline ELSE v.tag_baseline END`,
			workspaceID, connID, repo, branch, trigger, pullRequests, tagsRegex, tagBaseline)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET updated_at = now() WHERE id = ops_uuid($1)`, workspaceID); err != nil {
		return err
	}
	// The admin who attached sets vouched for the old repository and branch, not for the new one,
	// nor for unreviewed pull requests or tags, which may point at any commit.
	if detach {
		if _, err := tx.ExecContext(ctx, `DELETE FROM variable_set_workspaces WHERE workspace_id = ops_uuid($1)`, workspaceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VCSTriggered lists the remote workspaces whose branch pushes or pull requests queue runs.
func (s *Store) VCSTriggered(ctx context.Context) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+workspaceCols+` FROM `+workspaceFrom+`
		WHERE (vcs.trigger OR vcs.pull_requests) AND w.execution_mode = 'remote'`)
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
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	runs, err := queueRuns(ctx, tx, `SELECT w.id, $2::text, NULL::uuid, $3::text, NULLIF($4::integer, 0), $5::text FROM workspaces w
		WHERE w.id = ops_uuid($1) AND w.execution_mode = 'remote' AND NOT EXISTS (SELECT 1 FROM runs x
			WHERE x.workspace_id = w.id AND x.vcs_commit = $3
				AND CASE WHEN $4 > 0 THEN x.action = 'plan' AND x.vcs_pull_request = $4 ELSE x.action NOT IN ('assess', 'plan') END)`,
		workspaceID, action, commit, pullRequest, message)
	if err != nil || len(runs) == 0 {
		return false, err
	}
	id := runs[0].ID
	e, err := s.audit(ctx, tx, "vcs", "queue run", map[string]any{"principal": map[string]any{"type": "system"},
		"run": id, "workspace_id": workspaceID, "action": action, "commit": commit, "pull_request": pullRequest})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	e.stream()
	return true, nil
}

// PinVCSCommit records the commit a run plans, unless one is recorded already, and returns the
// run's commit: the apply phase gets the commit its plan was made from.
func (s *Store) PinVCSCommit(ctx context.Context, runID, commit string) (string, error) {
	err := s.DB.QueryRowContext(ctx, `UPDATE runs SET vcs_commit = COALESCE(vcs_commit, $2)
		WHERE id = ops_uuid($1) RETURNING vcs_commit`, runID, commit).Scan(&commit)
	return commit, err
}

// Settings are a workspace's settings pages: General (all fields) and Health (only the booleans).
type Settings struct {
	Name                      string // "" keeps it; a rename keeps state, runs and grants (keyed by id)
	AutoApply, DriftDetection bool   // apply plans without confirmation; check drift daily
	ExecutionMode             string // "remote" or "local"; "" keeps it
	// remote state sharing, only if Sharing: with all workspaces (Global) or with the Consumers
	// (workspace ids; unknown ids and the workspace itself are ignored)
	Sharing, Global bool
	Consumers       []string
}

// SaveSettings saves all of them or, on an error, none. ErrNameTaken: the new name is in use;
// sql.ErrNoRows: no such workspace.
func (s *Store) SaveSettings(ctx context.Context, id string, set Settings) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = affected(tx.ExecContext(ctx, `UPDATE workspaces SET name = COALESCE(NULLIF($2, ''), name), auto_apply = $3,
		drift_detection = $4, execution_mode = COALESCE(NULLIF($5, ''), execution_mode),
		global_remote_state = CASE WHEN $6 THEN $7 ELSE global_remote_state END, updated_at = now()
		WHERE id = ops_uuid($1)`, id, set.Name, set.AutoApply, set.DriftDetection, set.ExecutionMode, set.Sharing, set.Global))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrNameTaken
	} else if err != nil {
		return err
	}
	if set.Sharing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_state_consumers WHERE workspace_id = ops_uuid($1)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO remote_state_consumers (workspace_id, consumer_id)
			SELECT $1::uuid, id FROM workspaces WHERE id::text = ANY($2::text[]) AND id <> $1::uuid`, id, set.Consumers); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetExecutionMode sets a workspace's execution mode, "remote" or "local". Runs and CLI locks
// already under way finish; the mode only refuses new ones.
func (s *Store) SetExecutionMode(ctx context.Context, workspaceID, mode string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE workspaces SET execution_mode = $2, updated_at = now() WHERE id = ops_uuid($1)`, workspaceID, mode)
	return err
}

// RemoteStateConsumers lists the workspaces explicitly allowed to read a workspace's state, by name.
func (s *Store) RemoteStateConsumers(ctx context.Context, workspaceID string) ([]Workspace, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.name FROM remote_state_consumers c JOIN workspaces w ON w.id = c.consumer_id
		WHERE c.workspace_id = ops_uuid($1) ORDER BY w.name`, workspaceID)
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

// SharesState reports whether consumerID's runs may read workspaceID's state.
func (s *Store) SharesState(ctx context.Context, workspaceID, consumerID string) (bool, error) {
	var ok bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = ops_uuid($1) AND w.id::text <> $2
		AND (w.global_remote_state OR EXISTS (SELECT 1 FROM remote_state_consumers c WHERE c.workspace_id = w.id AND c.consumer_id = ops_uuid($2))))`,
		workspaceID, consumerID).Scan(&ok)
	return ok, err
}

// QueueAssessments queues an assess run (a refresh-only plan that is never applied) on every
// workspace with drift detection whose current state manages resources and that wasn't assessed
// within every. Workspaces with an active run are skipped; they get their turn at a later call.
func (s *Store) QueueAssessments(ctx context.Context, every time.Duration) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	runs, err := queueRuns(ctx, tx, `SELECT w.id, 'assess', NULL::uuid, NULL::text, NULL::integer,
			'drift detection: refresh-only plan queued, waiting for a runner'
		FROM workspaces w
		WHERE w.drift_detection AND w.execution_mode = 'remote'
			AND (EXISTS (SELECT 1 FROM workspace_vcs v WHERE v.workspace_id = w.id)
				OR EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id))
			AND EXISTS (SELECT 1 FROM state_version_resources r WHERE r.state_version_id =
				(SELECT max(v.id) FROM state_versions v WHERE v.workspace_id = w.id))
			AND NOT EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = w.id AND a.action = 'assess'
				AND a.created_at > now() - make_interval(secs => $1))`, every.Seconds())
	if err != nil {
		return 0, err
	}
	return len(runs), tx.Commit()
}

// Runs lists a workspace's runs, newest first; assessments (drift checks) are not listed.
func (s *Store) Runs(ctx context.Context, workspaceID string, limit int) ([]Run, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.workspace_id = ops_uuid($1) AND r.action <> 'assess'
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
	err := s.DB.QueryRowContext(ctx, `WITH r AS (SELECT id, status FROM runs WHERE workspace_id = ops_uuid($1) AND action <> 'assess'),
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
	return scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.workspace_id = ops_uuid($1)
		AND r.action = 'assess' ORDER BY r.created_at DESC LIMIT 1`, workspaceID))
}

// CurrentOutputs returns the outputs of the workspace's current state version. destroyed is true
// when the last successful run was a destroy.
func (s *Store) CurrentOutputs(ctx context.Context, workspaceID string) (outputs map[string]any, destroyed bool, err error) {
	var raw []byte
	err = s.DB.QueryRowContext(ctx, `SELECT
		(SELECT outputs FROM state_versions WHERE workspace_id = ops_uuid($1) ORDER BY id DESC LIMIT 1),
		COALESCE((SELECT action = 'destroy' AND remove_template IS NULL FROM runs WHERE workspace_id = ops_uuid($1) AND status = $2
			ORDER BY created_at DESC LIMIT 1), false)`, workspaceID, Applied).Scan(&raw, &destroyed)
	if err != nil || raw == nil {
		return nil, destroyed, err
	}
	return outputs, destroyed, json.Unmarshal(raw, &outputs)
}

// GetRun returns a run and its status history; sql.ErrNoRows if the id is unknown or not a uuid.
// RunStatus is a run with only its status: GetRun without the columns and history, for polling.
func (s *Store) RunStatus(ctx context.Context, id string) (Run, error) {
	var r Run
	err := s.DB.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ops_uuid($1)`, id).Scan(&r.Status)
	return r, err
}

func (s *Store) GetRun(ctx context.Context, id string) (Run, []StatusEntry, error) {
	r, err := scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs r WHERE r.id = ops_uuid($1)`, id))
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
	// checked here, not by the state machine: planning may also move to apply_queued (auto-apply)
	var cur string
	err = tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ops_uuid($1) FOR UPDATE`, id).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) || err == nil && cur != NeedsConfirmation {
		return ErrNotConfirmable
	}
	if err != nil {
		return err
	}
	if _, err := setStatus(ctx, tx, id, status, message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET confirmed_by = COALESCE(NULLIF($2, '')::uuid, confirmed_by)
		WHERE id = $1::uuid`, id, confirmedBy); err != nil {
		return err
	}
	return tx.Commit()
}
