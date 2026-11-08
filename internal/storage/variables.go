package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Variable is an environment or Terraform variable of a workspace or a variable set. Value is
// sealed with SECRET_KEY (see internal/vcs); Sensitive ones are never shown again.
type Variable struct {
	ID        string
	Category  string // env | terraform
	Key       string
	Value     []byte
	Sensitive bool
	Owner     VarOwner // part of the value's seal (vcs.VariableAD)
	UpdatedAt time.Time
}

// VarOwner is the workspace or the variable set (exactly one id set) variables belong to.
type VarOwner struct{ WorkspaceID, SetID string }

const ownerCond = `workspace_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid AND set_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid`

// Variables lists an owner's variables by category and key.
func (s *Store) Variables(ctx context.Context, o VarOwner) ([]Variable, error) {
	return scanVariables(s.DB.QueryContext(ctx, `SELECT `+varCols+` FROM variables v
		WHERE `+ownerCond+` ORDER BY category, key`, o.WorkspaceID, o.SetID))
}

const varCols = `v.id, v.category, v.key, v.value, v.sensitive, COALESCE(v.workspace_id::text, ''), COALESCE(v.set_id::text, ''), v.updated_at`

// AllVariables lists every variable, for re-sealing.
func (s *Store) AllVariables(ctx context.Context) ([]Variable, error) {
	return scanVariables(s.DB.QueryContext(ctx, `SELECT `+varCols+` FROM variables v`))
}

// AnyActiveRun reports whether a run is active anywhere: a SECRET_KEY rotation waits for none.
func (s *Store) AnyActiveRun(ctx context.Context) (bool, error) {
	var active bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE status IN `+activeRun+`)`).Scan(&active)
	return active, err
}

// ResealVariable replaces a variable's sealed value, unless it changed since it was read.
func (s *Store) ResealVariable(ctx context.Context, id string, old, sealed []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE variables SET value = $3 WHERE id = $1::uuid AND value = $2`, id, old, sealed)
	return err
}

func scanVariables(rows *sql.Rows, err error) ([]Variable, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Variable
	for rows.Next() {
		var v Variable
		if err := rows.Scan(&v.ID, &v.Category, &v.Key, &v.Value, &v.Sensitive, &v.Owner.WorkspaceID, &v.Owner.SetID, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetVariable adds a variable, or replaces the owner's variable with the same category and key.
func (s *Store) SetVariable(ctx context.Context, o VarOwner, v Variable) error {
	conflict := "(workspace_id, category, key)"
	if o.SetID != "" {
		conflict = "(set_id, category, key)"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO variables (workspace_id, set_id, category, key, value, sensitive)
		VALUES (NULLIF($1, '')::uuid, NULLIF($2, '')::uuid, $3, $4, $5, $6)
		ON CONFLICT `+conflict+` DO UPDATE SET value = EXCLUDED.value, sensitive = EXCLUDED.sensitive, updated_at = now()`,
		o.WorkspaceID, o.SetID, v.Category, v.Key, v.Value, v.Sensitive)
	return err
}

// DeleteVariable deletes one of the owner's variables; ids of other owners are ignored.
func (s *Store) DeleteVariable(ctx context.Context, o VarOwner, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM variables WHERE `+ownerCond+` AND id = ops_uuid($3)`, o.WorkspaceID, o.SetID, id)
	return err
}

// globalApplies: global sets apply to workspace $1 unless it's linked to a repository. That code
// is whatever a user chose to link, so only an admin attaching a set may give it credentials.
const globalApplies = `(vs.global AND NOT EXISTS (SELECT 1 FROM workspaces gw
	WHERE gw.id = $1::uuid AND gw.vcs_connection_id IS NOT NULL))`

// workspaceSets are the ids of the sets attached to workspace $1.
const workspaceSets = `(SELECT set_id FROM variable_set_workspaces WHERE workspace_id = $1::uuid)`

// projectSets are the ids of the sets attached to workspace $1's project, unless the workspace is
// linked to a repository: anyone who operates it may relink it, and a project set, unlike an
// attached one, isn't detached when the link changes.
const projectSets = `(SELECT sp.set_id FROM variable_set_projects sp JOIN workspaces pw ON pw.project_id = sp.project_id
	WHERE pw.id = $1::uuid AND pw.vcs_connection_id IS NULL)`

// RunVariables returns the variables a workspace's runs get, one per category and key. The
// workspace's own win over sets attached to it, those over sets attached to its project, those
// over global ones (project and global sets not for VCS-linked workspaces), and between sets of
// one kind the first by name wins.
func (s *Store) RunVariables(ctx context.Context, workspaceID string) ([]Variable, error) {
	return scanVariables(s.DB.QueryContext(ctx, `SELECT DISTINCT ON (v.category, v.key) `+varCols+`
		FROM variables v LEFT JOIN variable_sets vs ON vs.id = v.set_id
		WHERE v.workspace_id = $1::uuid OR `+globalApplies+` OR v.set_id IN `+workspaceSets+` OR v.set_id IN `+projectSets+`
		ORDER BY v.category, v.key, CASE WHEN v.workspace_id IS NOT NULL THEN 0 WHEN v.set_id IN `+workspaceSets+` THEN 1
			WHEN v.set_id IN `+projectSets+` THEN 2 ELSE 3 END, vs.name`, workspaceID))
}

// VariableSet is a reusable group of variables, applied to all workspaces (Global) or to the
// Workspaces and the workspaces of the Projects (names) it's attached to.
type VariableSet struct {
	ID, Name, Description string
	Global                bool
	Workspaces            []string
	Projects              []string
	CreatedAt             time.Time
}

const setCols = `vs.id, vs.name, vs.description, vs.global, vs.created_at,
	ARRAY(SELECT w.name FROM variable_set_workspaces a JOIN workspaces w ON w.id = a.workspace_id
		WHERE a.set_id = vs.id ORDER BY w.name),
	ARRAY(SELECT p.name FROM variable_set_projects a JOIN projects p ON p.id = a.project_id
		WHERE a.set_id = vs.id ORDER BY p.name)`

func scanSets(rows *sql.Rows, err error) ([]VariableSet, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VariableSet
	types := pgtype.NewMap()
	for rows.Next() {
		var v VariableSet
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.Global, &v.CreatedAt, types.SQLScanner(&v.Workspaces), types.SQLScanner(&v.Projects)); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateVariableSet returns ErrExists if the name is taken.
func (s *Store) CreateVariableSet(ctx context.Context, name, description string, global bool) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO variable_sets (name, description, global) VALUES ($1, $2, $3)`, name, description, global)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrExists
	}
	return err
}

func (s *Store) VariableSets(ctx context.Context) ([]VariableSet, error) {
	return scanSets(s.DB.QueryContext(ctx, `SELECT `+setCols+` FROM variable_sets vs ORDER BY vs.name`))
}

// WorkspaceVariableSets lists the sets that apply to a workspace: global ones (see globalApplies),
// and those attached to it or to its project (see projectSets).
func (s *Store) WorkspaceVariableSets(ctx context.Context, workspaceID string) ([]VariableSet, error) {
	return scanSets(s.DB.QueryContext(ctx, `SELECT `+setCols+` FROM variable_sets vs WHERE `+globalApplies+`
		OR vs.id IN `+workspaceSets+` OR vs.id IN `+projectSets+` ORDER BY vs.name`, workspaceID))
}

// ProjectVariableSets lists the sets attached to a project.
func (s *Store) ProjectVariableSets(ctx context.Context, projectID string) ([]VariableSet, error) {
	return scanSets(s.DB.QueryContext(ctx, `SELECT `+setCols+` FROM variable_sets vs
		WHERE vs.id IN (SELECT set_id FROM variable_set_projects WHERE project_id = ops_uuid($1)) ORDER BY vs.name`, projectID))
}

// GetVariableSet returns sql.ErrNoRows for an unknown name.
func (s *Store) GetVariableSet(ctx context.Context, name string) (VariableSet, error) {
	sets, err := scanSets(s.DB.QueryContext(ctx, `SELECT `+setCols+` FROM variable_sets vs WHERE vs.name = $1`, name))
	if err == nil && len(sets) == 0 {
		err = sql.ErrNoRows
	}
	if err != nil {
		return VariableSet{}, err
	}
	return sets[0], nil
}

func (s *Store) UpdateVariableSet(ctx context.Context, id, description string, global bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE variable_sets SET description = $2, global = $3 WHERE id = $1::uuid`, id, description, global)
	return err
}

// DeleteVariableSet deletes a set with its variables; workspaces keep their own.
func (s *Store) DeleteVariableSet(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM variable_sets WHERE id = $1::uuid`, id)
	return err
}

// AttachVariableSet applies a set to the named workspace, or stops applying it (attach=false).
// It returns sql.ErrNoRows for an unknown workspace.
func (s *Store) AttachVariableSet(ctx context.Context, setID, workspace string, attach bool) error {
	return s.attachTo(ctx, "variable_set_workspaces", "set_id", setID, "workspace", workspace, attach)
}

// AttachVariableSetProject applies a set to the workspaces of the named project, or stops (attach=false).
// It returns sql.ErrNoRows for an unknown project.
func (s *Store) AttachVariableSetProject(ctx context.Context, setID, project string, attach bool) error {
	return s.attachTo(ctx, "variable_set_projects", "set_id", setID, "project", project, attach)
}

// Selection is a join table from variable sets or VCS connections to workspaces or projects.
type Selection struct{ table, col, kind string }

var (
	SetWorkspaces  = Selection{"variable_set_workspaces", "set_id", "workspace"}
	SetProjects    = Selection{"variable_set_projects", "set_id", "project"}
	ConnWorkspaces = Selection{"vcs_connection_workspaces", "connection_id", "workspace"}
	ConnProjects   = Selection{"vcs_connection_projects", "connection_id", "project"}
)

// Pick is the names to select in a Selection, all others are deselected.
type Pick struct {
	Selection
	Names []string
}

// Select makes each pick's selection of id exactly its names, all or (on an error) none of them.
// sql.ErrNoRows: a name isn't a workspace or project.
func (s *Store) Select(ctx context.Context, id string, picks ...Pick) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range picks {
		names := p.Names // nil works too: NULL selects no names
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM unnest($1::text[]) n
			WHERE NOT EXISTS (SELECT 1 FROM `+p.kind+`s WHERE name = n))`, names).Scan(&ok); err != nil {
			return err
		} else if !ok {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+p.table+` WHERE `+p.col+` = $1::uuid
			AND `+p.kind+`_id NOT IN (SELECT id FROM `+p.kind+`s WHERE name = ANY($2))`, id, names); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+p.table+` (`+p.col+`, `+p.kind+`_id)
			SELECT $1::uuid, id FROM `+p.kind+`s WHERE name = ANY($2) ON CONFLICT DO NOTHING`, id, names); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// attachTo adds or removes the (col = id, <kind>_id) row of table, a join table to workspaces or
// projects (kind "workspace" or "project"; never input). sql.ErrNoRows: no such workspace or project.
func (s *Store) attachTo(ctx context.Context, table, col, id, kind, name string, attach bool) error {
	q := `INSERT INTO ` + table + ` (` + col + `, ` + kind + `_id) SELECT $1::uuid, id FROM ` + kind + `s WHERE name = $2
		ON CONFLICT DO NOTHING`
	if !attach {
		q = `DELETE FROM ` + table + ` WHERE ` + col + ` = $1::uuid AND ` + kind + `_id = (SELECT id FROM ` + kind + `s WHERE name = $2)`
	}
	if _, err := s.DB.ExecContext(ctx, q, id, name); err != nil {
		return err
	}
	var ok bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+kind+`s WHERE name = $1)`, name).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return sql.ErrNoRows
	}
	return nil
}
