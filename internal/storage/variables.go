package storage

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// Variable is an environment or Terraform variable of a workspace or a variable set. Value is
// sealed with SECRET_KEY (see internal/vcs); Sensitive ones are never shown again.
type Variable struct {
	ID        uuid.UUID
	Category  string // env | terraform
	Key       string
	Value     []byte
	Sensitive bool
	Owner     VarOwner // part of the value's seal (vcs.VariableAD)
	UpdatedAt time.Time
}

// VarOwner is the workspace or the variable set (exactly one id set, the other uuid.Nil) variables belong to.
type VarOwner struct{ WorkspaceID, SetID uuid.UUID }

func toVariables(rows []db.Variable, err error) ([]Variable, error) {
	out := make([]Variable, 0, len(rows))
	for _, v := range rows {
		out = append(out, Variable{ID: v.ID, Category: v.Category, Key: v.Key, Value: v.Value, Sensitive: v.Sensitive,
			Owner: VarOwner{WorkspaceID: v.WorkspaceID.UUID, SetID: v.SetID.UUID}, UpdatedAt: v.UpdatedAt})
	}
	return out, err
}

// Variables lists an owner's variables by category and key.
func (s *Store) Variables(ctx context.Context, o VarOwner) ([]Variable, error) {
	return toVariables(s.q.ListVariables(ctx, db.ListVariablesParams{WorkspaceID: optID(o.WorkspaceID), SetID: optID(o.SetID)}))
}

// AllVariables lists every variable, for re-sealing.
func (s *Store) AllVariables(ctx context.Context) ([]Variable, error) {
	return toVariables(s.q.AllVariables(ctx))
}

// ResealVariable replaces a variable's sealed value, unless it changed since it was read.
func (s *Store) ResealVariable(ctx context.Context, id uuid.UUID, old, sealed []byte) error {
	return s.q.ResealVariable(ctx, db.ResealVariableParams{ID: id, Old: old, Sealed: sealed})
}

// SetVariable adds a variable, or replaces the owner's variable with the same category and key.
func (s *Store) SetVariable(ctx context.Context, o VarOwner, v Variable) error {
	if o.SetID != uuid.Nil {
		return s.q.SetSetVariable(ctx, db.SetSetVariableParams{SetID: o.SetID, Category: v.Category, Key: v.Key, Value: v.Value, Sensitive: v.Sensitive})
	}
	return s.q.SetWorkspaceVariable(ctx, db.SetWorkspaceVariableParams{WorkspaceID: o.WorkspaceID, Category: v.Category, Key: v.Key,
		Value: v.Value, Sensitive: v.Sensitive})
}

// DeleteVariable deletes one of the owner's variables; ids of other owners are ignored.
func (s *Store) DeleteVariable(ctx context.Context, o VarOwner, id uuid.UUID) error {
	return s.q.DeleteVariable(ctx, db.DeleteVariableParams{WorkspaceID: optID(o.WorkspaceID), SetID: optID(o.SetID), ID: id})
}

// RunVariables returns the variables a workspace's runs get, one per category and key. The
// workspace's own win over sets attached to it, those over sets attached to its project, those
// over global ones (project and global sets not for VCS-linked workspaces; see variable_set_rank),
// and between sets of one kind the first by name wins.
func (s *Store) RunVariables(ctx context.Context, workspaceID uuid.UUID) ([]Variable, error) {
	return toVariables(s.q.RunVariables(ctx, workspaceID))
}

// VariableSet is a reusable group of variables, applied to all workspaces (Global) or to the
// Workspaces and the workspaces of the Projects (names) it's attached to.
type VariableSet struct {
	ID                uuid.UUID
	Name, Description string
	Global            bool
	Workspaces        []string
	Projects          []string
	CreatedAt         time.Time
}

func toSet(v db.VariableSetList) VariableSet {
	return VariableSet{ID: v.ID, Name: v.Name, Description: v.Description, Global: v.Global, Workspaces: v.Workspaces,
		Projects: v.Projects, CreatedAt: v.CreatedAt}
}

func toSets(rows []db.VariableSetList, err error) ([]VariableSet, error) {
	out := make([]VariableSet, 0, len(rows))
	for _, v := range rows {
		out = append(out, toSet(v))
	}
	return out, err
}

// CreateVariableSet returns ErrExists if the name is taken.
func (s *Store) CreateVariableSet(ctx context.Context, name, description string, global bool) error {
	err := s.q.CreateVariableSet(ctx, db.CreateVariableSetParams{Name: name, Description: description, Global: global})
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) VariableSets(ctx context.Context) ([]VariableSet, error) {
	return toSets(s.q.VariableSets(ctx))
}

// WorkspaceVariableSets lists the sets that apply to a workspace (variable_set_rank): global ones,
// and those attached to it or to its project.
func (s *Store) WorkspaceVariableSets(ctx context.Context, workspaceID uuid.UUID) ([]VariableSet, error) {
	return toSets(s.q.WorkspaceVariableSets(ctx, workspaceID))
}

// ProjectVariableSets lists the sets attached to a project.
func (s *Store) ProjectVariableSets(ctx context.Context, projectID uuid.UUID) ([]VariableSet, error) {
	return toSets(s.q.ProjectVariableSets(ctx, projectID))
}

// GetVariableSet returns ErrNotFound for an unknown name.
func (s *Store) GetVariableSet(ctx context.Context, name string) (VariableSet, error) {
	v, err := s.q.GetVariableSet(ctx, name)
	return toSet(v), notFound(err)
}

func (s *Store) UpdateVariableSet(ctx context.Context, id uuid.UUID, description string, global bool) error {
	return s.q.UpdateVariableSet(ctx, db.UpdateVariableSetParams{ID: id, Description: description, Global: global})
}

// DeleteVariableSet deletes a set with its variables; workspaces keep their own.
func (s *Store) DeleteVariableSet(ctx context.Context, id uuid.UUID) error {
	return s.q.DeleteVariableSet(ctx, id)
}

// AttachVariableSet applies a set to the named workspace, or stops applying it (attach=false).
// It returns ErrNotFound for an unknown workspace.
func (s *Store) AttachVariableSet(ctx context.Context, setID uuid.UUID, workspace string, attach bool) error {
	var err error
	if attach {
		err = s.q.AttachSetWorkspace(ctx, db.AttachSetWorkspaceParams{SetID: setID, Name: workspace})
	} else {
		err = s.q.DetachSetWorkspace(ctx, db.DetachSetWorkspaceParams{SetID: setID, Name: workspace})
	}
	if err != nil {
		return err
	}
	return exists(s.q.WorkspacesExist(ctx, []string{workspace}))
}

// AttachVariableSetProject applies a set to the workspaces of the named project, or stops (attach=false).
// It returns ErrNotFound for an unknown project.
func (s *Store) AttachVariableSetProject(ctx context.Context, setID uuid.UUID, project string, attach bool) error {
	var err error
	if attach {
		err = s.q.AttachSetProject(ctx, db.AttachSetProjectParams{SetID: setID, Name: project})
	} else {
		err = s.q.DetachSetProject(ctx, db.DetachSetProjectParams{SetID: setID, Name: project})
	}
	if err != nil {
		return err
	}
	return exists(s.q.ProjectsExist(ctx, []string{project}))
}

// exists turns false into ErrNotFound.
func exists(ok bool, err error) error {
	if err == nil && !ok {
		return ErrNotFound
	}
	return err
}

// Selection is a join table from variable sets or VCS connections to workspaces or projects.
type Selection int

const (
	SetWorkspaces Selection = iota
	SetProjects
	ConnWorkspaces
	ConnProjects
)

// Pick is the names to select in a Selection, all others are deselected.
type Pick struct {
	Selection
	Names []string
}

// Select makes each pick's selection of id (a variable set's or a VCS connection's) exactly its
// names, all or (on an error) none of them. ErrNotFound: a name isn't a workspace or project.
func (s *Store) Select(ctx context.Context, id uuid.UUID, picks ...Pick) error {
	return s.tx(ctx, func(q *db.Queries) error {
		for _, p := range picks {
			names := p.Names // nil works too: NULL selects no names
			var err error
			switch p.Selection {
			case SetWorkspaces, ConnWorkspaces:
				err = exists(q.WorkspacesExist(ctx, names))
			default:
				err = exists(q.ProjectsExist(ctx, names))
			}
			if err != nil {
				return err
			}
			switch p.Selection {
			case SetWorkspaces:
				err = q.SelectSetWorkspaces(ctx, db.SelectSetWorkspacesParams{ID: id, Names: names})
			case SetProjects:
				err = q.SelectSetProjects(ctx, db.SelectSetProjectsParams{ID: id, Names: names})
			case ConnWorkspaces:
				err = q.SelectConnWorkspaces(ctx, db.SelectConnWorkspacesParams{ID: id, Names: names})
			case ConnProjects:
				err = q.SelectConnProjects(ctx, db.SelectConnProjectsParams{ID: id, Names: names})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}
