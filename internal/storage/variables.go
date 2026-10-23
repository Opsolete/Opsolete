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
	UpdatedAt time.Time
}

// VarOwner is the workspace or the variable set (exactly one id set) variables belong to.
type VarOwner struct{ WorkspaceID, SetID string }

const ownerCond = `workspace_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid AND set_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid`

// Variables lists an owner's variables by category and key.
func (s *Store) Variables(ctx context.Context, o VarOwner) ([]Variable, error) {
	return scanVariables(s.DB.QueryContext(ctx, `SELECT id, category, key, value, sensitive, updated_at FROM variables
		WHERE `+ownerCond+` ORDER BY category, key`, o.WorkspaceID, o.SetID))
}

func scanVariables(rows *sql.Rows, err error) ([]Variable, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Variable
	for rows.Next() {
		var v Variable
		if err := rows.Scan(&v.ID, &v.Category, &v.Key, &v.Value, &v.Sensitive, &v.UpdatedAt); err != nil {
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
	_, err := s.DB.ExecContext(ctx, `DELETE FROM variables WHERE `+ownerCond+` AND id::text = $3`, o.WorkspaceID, o.SetID, id)
	return err
}

// globalApplies: global sets apply to workspace $1 unless it's linked to a repository. That code
// is whatever a user chose to link, so only an admin attaching a set may give it credentials.
const globalApplies = `(vs.global AND NOT EXISTS (SELECT 1 FROM workspaces gw
	WHERE gw.id = $1::uuid AND gw.vcs_connection_id IS NOT NULL))`

// RunVariables returns the variables a workspace's runs get, one per category and key. The
// workspace's own win over attached sets, attached sets over global ones (not for VCS-linked
// workspaces, see globalApplies), and between sets of one kind the first by name wins.
func (s *Store) RunVariables(ctx context.Context, workspaceID string) ([]Variable, error) {
	return scanVariables(s.DB.QueryContext(ctx, `SELECT DISTINCT ON (v.category, v.key) v.id, v.category, v.key, v.value,
			v.sensitive, v.updated_at
		FROM variables v LEFT JOIN variable_sets vs ON vs.id = v.set_id
		WHERE v.workspace_id = $1::uuid OR `+globalApplies+`
			OR v.set_id IN (SELECT set_id FROM variable_set_workspaces WHERE workspace_id = $1::uuid)
		ORDER BY v.category, v.key, v.workspace_id IS NULL, vs.global, vs.name`, workspaceID))
}

// VariableSet is a reusable group of variables, applied to all workspaces (Global) or to the
// Workspaces (names) it's attached to.
type VariableSet struct {
	ID, Name, Description string
	Global                bool
	Workspaces            []string
	CreatedAt             time.Time
}

const setCols = `vs.id, vs.name, vs.description, vs.global, vs.created_at,
	ARRAY(SELECT w.name FROM variable_set_workspaces a JOIN workspaces w ON w.id = a.workspace_id
		WHERE a.set_id = vs.id ORDER BY w.name)`

func scanSets(rows *sql.Rows, err error) ([]VariableSet, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VariableSet
	types := pgtype.NewMap()
	for rows.Next() {
		var v VariableSet
		if err := rows.Scan(&v.ID, &v.Name, &v.Description, &v.Global, &v.CreatedAt, types.SQLScanner(&v.Workspaces)); err != nil {
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

// WorkspaceVariableSets lists the sets that apply to a workspace: global (see globalApplies) and attached ones.
func (s *Store) WorkspaceVariableSets(ctx context.Context, workspaceID string) ([]VariableSet, error) {
	return scanSets(s.DB.QueryContext(ctx, `SELECT `+setCols+` FROM variable_sets vs WHERE `+globalApplies+`
		OR vs.id IN (SELECT set_id FROM variable_set_workspaces WHERE workspace_id = $1::uuid) ORDER BY vs.name`, workspaceID))
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
	q := `INSERT INTO variable_set_workspaces (set_id, workspace_id) SELECT $1::uuid, id FROM workspaces WHERE name = $2
		ON CONFLICT DO NOTHING`
	if !attach {
		q = `DELETE FROM variable_set_workspaces WHERE set_id = $1::uuid AND workspace_id = (SELECT id FROM workspaces WHERE name = $2)`
	}
	if _, err := s.DB.ExecContext(ctx, q, setID, workspace); err != nil {
		return err
	}
	var ok bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE name = $1)`, workspace).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return sql.ErrNoRows
	}
	return nil
}
