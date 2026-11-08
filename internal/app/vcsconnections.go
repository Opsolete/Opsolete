package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// VCS connections are GitHub or Azure DevOps accounts (an access token) that admins add. Workspaces
// within a connection's workspace scope link its repositories (see LinkVCS), registry modules publish
// from them (PublishVCSModule) and the template catalog may live in one. Admins only, all of it.

var (
	errCatalogConn = reason{storage.ErrInUse, "The template catalog uses this connection: disconnect it in Settings → Templates first."}
	errLinkedRun   = reason{storage.ErrRunActive, "a workspace linked to this connection has an active run: wait for it to finish, or discard or cancel it"}
	errTokenRace   = reason{ErrChanged, "the token was replaced or the connection deleted meanwhile: reload and try again"}
)

// VCSConnections lists the connections with what uses them. Admins only.
func (a *App) VCSConnections(ctx context.Context, actor Actor) ([]storage.VCSConnection, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	return a.Store.VCSConnections(ctx)
}

// VCSConnection loads a connection. Admins only; ErrNotFound.
func (a *App) VCSConnection(ctx context.Context, actor Actor, id uuid.UUID) (storage.VCSConnection, error) {
	if !actor.Admin {
		return storage.VCSConnection{}, errAdminsOnly
	}
	return a.Store.GetVCSConnection(ctx, id)
}

// NewVCSConnection is a connection to add, as typed.
type NewVCSConnection struct {
	Name, Provider, BaseURL string // BaseURL "" is the provider's default
	Token                   string
}

// checkToken is the connection check: the provider lists repositories with the token.
func (a *App) checkToken(ctx context.Context, provider, baseURL, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := a.VCS.Conn(provider, baseURL, token).Repos(ctx); err != nil {
		return invalid("token", "connection check failed: "+err.Error())
	}
	return nil
}

// CreateVCSConnection checks the token against the provider and stores it sealed to the connection's
// endpoint (app.TokenAD). A new connection is open to no workspace (see SetVCSScope). Admins only;
// ValidationError: name (also when taken), provider or base URL, token (also when the check fails).
func (a *App) CreateVCSConnection(ctx context.Context, actor Actor, in NewVCSConnection) (uuid.UUID, error) {
	if !actor.Admin {
		return uuid.Nil, errAdminsOnly
	}
	in.Name, in.Provider, in.BaseURL, in.Token = strings.TrimSpace(in.Name), strings.TrimSpace(in.Provider), strings.TrimSpace(in.BaseURL), strings.TrimSpace(in.Token)
	if in.Name == "" || len(in.Name) > 64 || in.Token == "" {
		return uuid.Nil, invalid("name", "name (up to 64 characters) and access token are required")
	}
	baseURL, err := vcs.NormalizeURL(in.Provider, in.BaseURL)
	if err != nil {
		return uuid.Nil, invalid("base_url", err.Error())
	}
	if err := a.checkToken(ctx, in.Provider, baseURL, in.Token); err != nil {
		return uuid.Nil, err
	}
	conn := storage.VCSConnection{Name: in.Name, Provider: in.Provider, BaseURL: baseURL}
	if conn.Token, err = a.Secrets.Seal(in.Token, TokenAD(conn)); err != nil {
		return uuid.Nil, err
	}
	id, err := a.Store.CreateVCSConnection(ctx, conn)
	if errors.Is(err, storage.ErrExists) {
		return uuid.Nil, invalid("name", "a connection with this name already exists")
	}
	if err != nil {
		return uuid.Nil, err
	}
	a.Log.Info("vcs connection added", "name", in.Name, "provider", in.Provider, "url", baseURL, "user", actor.Email)
	return id, nil
}

// ReplaceVCSToken swaps a connection's token once the provider accepts it (the connection check);
// everything that uses the connection uses the new token from then on, and the template catalog, if
// it uses the connection, reloads with it right away. It returns the connection. Admins only;
// ValidationError: the token; ErrChanged: the token changed or the connection was deleted meanwhile.
func (a *App) ReplaceVCSToken(ctx context.Context, actor Actor, id uuid.UUID, token string) (storage.VCSConnection, error) {
	sc, err := a.VCSConnection(ctx, actor, id)
	if err != nil {
		return sc, err
	}
	if token = strings.TrimSpace(token); token == "" {
		return sc, invalid("token", "paste the new access token")
	}
	if err := a.checkToken(ctx, sc.Provider, sc.BaseURL, token); err != nil {
		return sc, err
	}
	sealed, err := a.Secrets.Seal(token, TokenAD(sc))
	if err == nil {
		err = a.Store.ResealVCSToken(ctx, sc.ID, sc.Token, sealed)
	}
	if errors.Is(err, storage.ErrNotFound) {
		return sc, errTokenRace
	}
	if err != nil {
		return sc, err
	}
	a.Log.Info("vcs token replaced", "connection", sc.Name, "user", actor.Email)
	if sc.Catalog {
		a.syncCatalog(ctx)
	}
	return sc, nil
}

// VCSScope is which workspaces may link a connection's repositories.
type VCSScope struct {
	Scope                string   // "" none, "all" or "selected"
	Workspaces, Projects []string // by name, for "selected": exactly these (other scopes keep the selection for later)
}

// SetVCSScope sets which workspaces may link a connection. Workspaces linked before that the scope no
// longer allows keep their link, but their runs fail when they start and the poller skips them
// (storage.VCSConnection.Allows). Admins only; ValidationError: unknown workspaces or projects.
func (a *App) SetVCSScope(ctx context.Context, actor Actor, id uuid.UUID, in VCSScope) error {
	sc, err := a.VCSConnection(ctx, actor, id)
	if err != nil {
		return err
	}
	switch in.Scope {
	case "", "all":
	case "selected":
		err = a.Store.Select(ctx, sc.ID, storage.Pick{Selection: storage.ConnWorkspaces, Names: in.Workspaces},
			storage.Pick{Selection: storage.ConnProjects, Names: in.Projects})
	default:
		return invalid("workspace_scope", "the workspace scope is none, all or selected")
	}
	if errors.Is(err, storage.ErrNotFound) {
		return &ValidationError{Message: "choose existing workspaces and projects"}
	}
	if err == nil {
		err = a.Store.SetVCSScope(ctx, sc.ID, in.Scope)
	}
	if err != nil {
		return err
	}
	a.Log.Info("vcs connection scope", "id", sc.ID, "workspace_scope", in.Scope, "workspaces", in.Workspaces, "projects", in.Projects, "user", actor.Email)
	return nil
}

// DeleteVCSConnection deletes a connection. What used it:
//   - the template catalog: refused (a conflict) until the catalog is disconnected;
//   - linked workspaces: unlinked (they keep their variables and added templates; runs need a new
//     source); refused (a conflict, storage.ErrRunActive) while one has an active run, as relinking it;
//   - registry modules published from it: unlinked, their published versions stay.
//
// Deleting an unknown connection deletes nothing and succeeds. Admins only.
func (a *App) DeleteVCSConnection(ctx context.Context, actor Actor, id uuid.UUID) error {
	sc, err := a.VCSConnection(ctx, actor, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	err = a.Store.DeleteVCSConnection(ctx, sc.ID)
	switch {
	case errors.Is(err, storage.ErrInUse):
		return errCatalogConn
	case errors.Is(err, storage.ErrRunActive):
		return errLinkedRun
	}
	if err != nil {
		return err
	}
	a.Log.Info("vcs connection deleted", "id", sc.ID, "name", sc.Name, "unlinked_workspaces", sc.LinkedWorkspaces,
		"unlinked_modules", sc.LinkedModules, "user", actor.Email)
	return nil
}

// VCSRepos lists the repositories a connection's token can read, for pickers. Admins only;
// other errors are the provider's (typing a repository still works).
func (a *App) VCSRepos(ctx context.Context, actor Actor, id uuid.UUID) ([]string, error) {
	sc, err := a.VCSConnection(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	c, err := a.VCS.ConnFor(a.OpenToken, sc)
	if err != nil {
		return nil, err
	}
	repos, err := c.Repos(ctx)
	if err != nil {
		return nil, errors.New("could not list repositories: " + err.Error())
	}
	return repos, nil
}
