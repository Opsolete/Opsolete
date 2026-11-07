package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TemplateUse is one place a template is used: the root configuration of a workspace created from
// it (Module ""), or a template added to a workspace (module.<Module>).
type TemplateUse struct {
	WorkspaceID, Workspace, ProjectName, ExecutionMode string
	Module                                             string
	ModuleSource, ModuleRef                            string
	Inputs                                             map[string]any
	RunID, RunStatus                                   string // the workspace's active run; "" if none
}

// TemplateUses lists every use of template templateID, by workspace name and module; projectID
// limits them to one project, "" lists all.
func (s *Store) TemplateUses(ctx context.Context, templateID, projectID string) ([]TemplateUse, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT w.id, w.name, (SELECT p.name FROM projects p WHERE p.id = w.project_id), w.execution_mode,
			u.module, u.module_source, u.module_ref, u.inputs, COALESCE(r.id::text, ''), COALESCE(r.status, '')
		FROM (SELECT id AS workspace_id, '' AS module, module_source, module_ref, inputs FROM workspaces WHERE template_id = $1
			UNION ALL SELECT workspace_id, name, module_source, module_ref, inputs FROM workspace_templates WHERE template_id = $1) u
		JOIN workspaces w ON w.id = u.workspace_id
		LEFT JOIN runs r ON r.workspace_id = w.id AND r.status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
		WHERE $2 = '' OR w.project_id::text = $2
		ORDER BY w.name, u.module`, templateID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateUse
	for rows.Next() {
		var u TemplateUse
		var raw []byte
		if err := rows.Scan(&u.WorkspaceID, &u.Workspace, &u.ProjectName, &u.ExecutionMode, &u.Module, &u.ModuleSource, &u.ModuleRef, &raw,
			&u.RunID, &u.RunStatus); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &u.Inputs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TemplateUpgrade moves one use of template TemplateID (Module "" = the root) from the version and
// inputs it was read with (From) to a module version and inputs.
type TemplateUpgrade struct {
	TemplateID, Module, ModuleSource, ModuleRef string
	Inputs                                      map[string]any
	From                                        TemplateUse
}

// ErrRolloutClosed: the rollout was paused, stopped or finished meanwhile.
var ErrRolloutClosed = errors.New("the rollout is no longer active")

// UpgradeTemplates applies ups, all of one workspace, and queues one apply run for them as part of
// rollout rolloutID, which waits for confirmation even with auto-apply. If a use changed meanwhile
// (sql.ErrNoRows), there is no free workspace (ErrRunActive, ErrLocalExecution) or the rollout is
// no longer active (ErrRolloutClosed), nothing changes. If the run ends before its apply (e.g. its
// plan is discarded), the uses go back (configChange), and a resumed rollout retries them.
func (s *Store) UpgradeTemplates(ctx context.Context, workspaceID string, ups []TemplateUpgrade, createdBy, rolloutID string) (Run, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	var status string // FOR SHARE: a pause or stop waits until this run is queued, and is seen by the next
	if err := tx.QueryRowContext(ctx, `SELECT status FROM template_rollouts WHERE id = ops_uuid($1) FOR SHARE`, rolloutID).Scan(&status); err != nil {
		return Run{}, err
	}
	if status != "active" {
		return Run{}, ErrRolloutClosed
	}
	var changes []configChange
	for _, u := range ups {
		before, err := configSnapshot(ctx, tx, workspaceID, u.Module)
		if err != nil {
			return Run{}, err
		}
		changes = append(changes, configChange{Module: u.Module, Before: before})
		raw, err := json.Marshal(u.Inputs)
		if err != nil {
			return Run{}, err
		}
		old, err := json.Marshal(u.From.Inputs)
		if err != nil {
			return Run{}, err
		}
		// only if it still runs what the new inputs were made from: a change meanwhile isn't overwritten
		q := `UPDATE workspace_templates SET module_source = $4, module_ref = $5, inputs = $6
			WHERE workspace_id = $1::uuid AND name = $2 AND template_id = $3 AND module_source = $7 AND module_ref = $8 AND inputs = $9::jsonb`
		if u.Module == "" {
			q = `UPDATE workspaces SET module_source = $4, module_ref = $5, inputs = $6, updated_at = now()
				WHERE id = $1::uuid AND $2::text = '' AND template_id = $3 AND module_source = $7 AND module_ref = $8 AND inputs = $9::jsonb`
		}
		res, err := tx.ExecContext(ctx, q, workspaceID, u.Module, u.TemplateID, u.ModuleSource, u.ModuleRef, raw,
			u.From.ModuleSource, u.From.ModuleRef, old)
		if err != nil {
			return Run{}, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return Run{}, err
		} else if n == 0 {
			return Run{}, sql.ErrNoRows
		}
	}
	r, err := createRun(ctx, tx, workspaceID, "apply", createdBy)
	if err != nil {
		return Run{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET rollout_id = $2 WHERE id = $1`, r.ID, rolloutID); err != nil {
		return Run{}, err
	}
	if err := recordConfigChanges(ctx, tx, r.ID, workspaceID, changes); err != nil {
		return Run{}, err
	}
	return r, tx.Commit()
}

// Rollout moves the workspaces using a template to ModuleSource/ModuleRef, the template's version
// when it started: at most BatchSize workspaces with an open rollout run at a time (0: no limit).
type Rollout struct {
	ID, TemplateID, ModuleSource, ModuleRef string
	ProjectID, ProjectName                  string // "": every project
	BatchSize                               int
	Status                                  string // active | paused | done | stopped
	Message                                 string
	CreatedBy, CreatedByEmail               string
	CreatedAt, ResumedAt                    time.Time
}

// Open: the rollout may still queue runs (paused ones once resumed).
func (r Rollout) Open() bool { return r.Status == "active" || r.Status == "paused" }

const rolloutCols = `o.id, o.template_id, o.module_source, o.module_ref, COALESCE(o.project_id::text, ''),
	COALESCE((SELECT p.name FROM projects p WHERE p.id = o.project_id), ''), o.batch_size, o.status, o.message,
	COALESCE(o.created_by::text, ''), COALESCE((SELECT email FROM users u WHERE u.id = o.created_by), ''), o.created_at, o.resumed_at`

func scanRollout(row interface{ Scan(...any) error }) (Rollout, error) {
	var o Rollout
	err := row.Scan(&o.ID, &o.TemplateID, &o.ModuleSource, &o.ModuleRef, &o.ProjectID, &o.ProjectName, &o.BatchSize, &o.Status,
		&o.Message, &o.CreatedBy, &o.CreatedByEmail, &o.CreatedAt, &o.ResumedAt)
	return o, err
}

// CreateRollout starts a rollout; ErrExists if the template has an open one. Its first turn is the
// caller's (ticked_at now): DueRollouts doesn't hand it out meanwhile, so the batch limit holds.
func (s *Store) CreateRollout(ctx context.Context, o Rollout) (Rollout, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO template_rollouts (template_id, module_source, module_ref, project_id, batch_size, created_by, ticked_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, NULLIF($6, '')::uuid, now()) RETURNING id`,
		o.TemplateID, o.ModuleSource, o.ModuleRef, o.ProjectID, o.BatchSize, o.CreatedBy).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return Rollout{}, ErrExists
	}
	if err != nil {
		return Rollout{}, err
	}
	return scanRollout(s.DB.QueryRowContext(ctx, `SELECT `+rolloutCols+` FROM template_rollouts o WHERE o.id = $1`, id))
}

// LatestRollout returns a template's most recent rollout; sql.ErrNoRows if it never had one.
func (s *Store) LatestRollout(ctx context.Context, templateID string) (Rollout, error) {
	return scanRollout(s.DB.QueryRowContext(ctx, `SELECT `+rolloutCols+` FROM template_rollouts o
		WHERE o.template_id = $1 ORDER BY o.created_at DESC LIMIT 1`, templateID))
}

// DueRollouts claims the active rollouts no app instance worked on within every, so instances take turns.
// shortcut: a turn that takes longer than every may overlap the next instance's; the batch limit can then be exceeded once.
func (s *Store) DueRollouts(ctx context.Context, every time.Duration) ([]Rollout, error) {
	rows, err := s.DB.QueryContext(ctx, `WITH due AS (
			UPDATE template_rollouts SET ticked_at = now() WHERE status = 'active'
				AND (ticked_at IS NULL OR ticked_at < now() - make_interval(secs => $1)) RETURNING id)
		SELECT `+rolloutCols+` FROM template_rollouts o JOIN due ON due.id = o.id ORDER BY o.created_at`, every.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rollout
	for rows.Next() {
		o, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetRolloutStatus changes an open rollout's status; reports false if it's no longer open. Resuming
// (active) forgets the run failures before, and its next turn is the caller's, as with CreateRollout.
func (s *Store) SetRolloutStatus(ctx context.Context, id, status, message string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE template_rollouts SET status = $2, message = $3,
			resumed_at = CASE WHEN $2 = 'active' THEN now() ELSE resumed_at END,
			ticked_at = CASE WHEN $2 = 'active' THEN now() ELSE ticked_at END
		WHERE id = ops_uuid($1) AND status IN ('active', 'paused')`, id, status, message)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RolloutRuns counts a rollout's open runs, and describes the first of its runs that errored, was
// discarded or canceled after since ("" if none).
func (s *Store) RolloutRuns(ctx context.Context, id string, since time.Time) (open int, failed string, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM runs WHERE rollout_id = ops_uuid($1)
				AND status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')),
			COALESCE((SELECT 'the run in ' || w.name || ': ' || replace(r.status, '_', ' ') FROM runs r
				JOIN workspaces w ON w.id = r.workspace_id
				WHERE r.rollout_id = ops_uuid($1) AND r.status IN ('errored', 'discarded', 'canceled') AND r.updated_at > $2
				ORDER BY r.updated_at LIMIT 1), '')`, id, since).Scan(&open, &failed)
	return open, failed, err
}
