package storage

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// VCSConnection is a VCS provider account; Token is sealed (see internal/vcs).
type VCSConnection struct {
	ID                      uuid.UUID
	Name, Provider, BaseURL string
	Token                   []byte
	WorkspaceScope          string   // workspaces that may link its repositories: "" none, "all" or "selected"
	Workspaces              []string // names of the selected workspaces
	Projects                []string // names of the selected projects: all their workspaces may link
	CreatedAt               time.Time
	// what uses it, for the VCS providers page
	LinkedWorkspaces, LinkedModules int
	Catalog                         bool // the template catalog's repository
}

// Allows reports whether the named workspace ("" for one being created) in the named project may
// link the connection's repositories.
func (c VCSConnection) Allows(workspace, project string) bool {
	return c.WorkspaceScope == "all" || c.WorkspaceScope == "selected" &&
		(workspace != "" && slices.Contains(c.Workspaces, workspace) || slices.Contains(c.Projects, project))
}

// ResealVCSToken replaces a connection's sealed token, unless it changed since it was read
// (ErrNotFound, also for a deleted connection).
func (s *Store) ResealVCSToken(ctx context.Context, id uuid.UUID, old, sealed []byte) error {
	return affected(s.q.ResealVCSToken(ctx, db.ResealVCSTokenParams{ID: id, Old: old, Sealed: sealed}))
}

// CreateVCSConnection returns the new connection's id; ErrExists if the name is taken.
func (s *Store) CreateVCSConnection(ctx context.Context, c VCSConnection) (uuid.UUID, error) {
	id, err := s.q.CreateVCSConnection(ctx, db.CreateVCSConnectionParams{Name: c.Name, Provider: c.Provider, BaseUrl: c.BaseURL, Token: c.Token})
	if isUnique(err) {
		return uuid.Nil, ErrExists
	}
	return id, err
}

// toConnection maps a vcs_connection_list row (001_schema.sql).
func toConnection(c db.VcsConnectionList) VCSConnection {
	return VCSConnection{ID: c.ID, Name: c.Name, Provider: c.Provider, BaseURL: c.BaseUrl, Token: c.Token, WorkspaceScope: c.WorkspaceScope,
		Workspaces: c.Workspaces, Projects: c.Projects, CreatedAt: c.CreatedAt, LinkedWorkspaces: int(c.LinkedWorkspaces),
		LinkedModules: int(c.LinkedModules), Catalog: c.Catalog}
}

func (s *Store) VCSConnections(ctx context.Context) ([]VCSConnection, error) {
	rows, err := s.q.VCSConnections(ctx)
	out := make([]VCSConnection, 0, len(rows))
	for _, c := range rows {
		out = append(out, toConnection(c))
	}
	return out, err
}

// GetVCSConnection returns ErrNotFound if the id is unknown.
func (s *Store) GetVCSConnection(ctx context.Context, id uuid.UUID) (VCSConnection, error) {
	c, err := s.q.GetVCSConnection(ctx, id)
	return toConnection(c), notFound(err)
}

// SetVCSScope sets the workspaces that may link a connection's repositories: "", "all" or "selected".
func (s *Store) SetVCSScope(ctx context.Context, id uuid.UUID, workspaceScope string) error {
	return s.q.SetVCSScope(ctx, db.SetVCSScopeParams{ID: id, WorkspaceScope: workspaceScope})
}

// DeleteVCSConnection also unlinks its workspaces and modules (their published versions stay). It
// returns ErrRunActive while a linked workspace has an active run (its next job would fetch from
// the connection), ErrInUse while the template catalog uses the connection.
func (s *Store) DeleteVCSConnection(ctx context.Context, id uuid.UUID) error {
	err := s.tx(ctx, func(q *db.Queries) error {
		if err := q.LockConnectionWorkspaces(ctx, id); err != nil {
			return err
		}
		active, err := q.ConnectionHasActiveRun(ctx, id)
		if err != nil {
			return err
		}
		if active {
			return ErrRunActive
		}
		return q.DeleteVCSConnection(ctx, id)
	})
	if _, ok := fkViolation(err); ok {
		return ErrInUse
	}
	return err
}

// VCSModule links a registry module to a repository whose semver tags become its versions.
type VCSModule struct {
	Namespace, Name, System string
	ConnectionID            uuid.UUID
	Connection, Provider    string // name and provider of the connection, for display
	Repo                    string
	Dir                     string // folder of the module in the repository; "" = the root
	SyncedAt                *time.Time
	SyncError               string
}

// LinkVCSModule returns ErrExists if the module is already linked to a repo.
func (s *Store) LinkVCSModule(ctx context.Context, m VCSModule) error {
	err := s.q.LinkVCSModule(ctx, db.LinkVCSModuleParams{Namespace: m.Namespace, Name: m.Name, System: m.System,
		ConnectionID: m.ConnectionID, Repo: m.Repo, Dir: m.Dir})
	if isUnique(err) {
		return ErrExists
	}
	return err
}

func toVCSModule(m db.VCSModulesRow) VCSModule {
	return VCSModule{Namespace: m.Namespace, Name: m.Name, System: m.System, ConnectionID: m.ConnectionID, Connection: m.Connection,
		Provider: m.Provider, Repo: m.Repo, Dir: m.Dir, SyncedAt: m.SyncedAt, SyncError: m.SyncError}
}

func (s *Store) VCSModules(ctx context.Context) ([]VCSModule, error) {
	rows, err := s.q.VCSModules(ctx)
	out := make([]VCSModule, 0, len(rows))
	for _, m := range rows {
		out = append(out, toVCSModule(m))
	}
	return out, err
}

// GetVCSModule returns ErrNotFound if the module isn't linked to a repo.
func (s *Store) GetVCSModule(ctx context.Context, namespace, name, system string) (VCSModule, error) {
	m, err := s.q.GetVCSModule(ctx, db.GetVCSModuleParams{Namespace: namespace, Name: name, System: system})
	return toVCSModule(db.VCSModulesRow(m)), notFound(err)
}

// UnlinkVCSModule stops publishing a module from its repository; published versions stay.
// It returns ErrNotFound if the module isn't linked.
func (s *Store) UnlinkVCSModule(ctx context.Context, namespace, name, system string) error {
	return affected(s.q.UnlinkVCSModule(ctx, db.UnlinkVCSModuleParams{Namespace: namespace, Name: name, System: system}))
}

// SetVCSSync records the outcome of a sync; syncErr "" means it succeeded.
func (s *Store) SetVCSSync(ctx context.Context, namespace, name, system, syncErr string) error {
	return s.q.SetVCSSync(ctx, db.SetVCSSyncParams{Namespace: namespace, Name: name, System: system, SyncError: syncErr})
}
