// Package app holds Opsolete's operations: access checks, sealing and the other rules every surface
// (the UI, the cloud API, a future SPA) shares. Surfaces decode a request, call an operation with the
// Actor who asked, and map its error (see errors.go).
package app

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"gocloud.dev/blob"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

type App struct {
	Store *storage.Store
	// Blobs is the state store: state files and run artifacts, sealed (any gocloud.dev/blob URL).
	Blobs     *blob.Bucket
	SecretKey []byte // from SECRET_KEY: seals template inputs, variables, VCS tokens and blobs
	Templates *templates.Catalog
	Registry  *registry.Registry
	// RegistryEnabled: templates pinning a module of this registry take their inputs from its variables.
	RegistryEnabled bool
	Log             *slog.Logger
}

// Actor is who performs an operation. A system actor (VCS poller, rollouts) has UserID uuid.Nil,
// an Email naming it (e.g. "vcs") and Admin true: server code acting on its own isn't access-checked.
type Actor struct {
	UserID uuid.UUID
	Email  string
	Admin  bool
}

func (a Actor) viewer() storage.Viewer { return storage.Viewer{UserID: a.UserID, Admin: a.Admin} }

// Level is what an actor may do in a workspace, ordered: LevelOperate includes LevelRead. Admins
// and a workspace's creator operate it; groups get read or operate per workspace or project.
type Level int

const (
	LevelNone Level = iota
	LevelRead
	LevelOperate
)

// String is the level as storage and forms name it: "", "read" or "operate".
func (l Level) String() string { return [...]string{"", "read", "operate"}[l] }

// ParseLevel reads "", "read" or "operate".
func ParseLevel(s string) (Level, bool) {
	switch s {
	case "":
		return LevelNone, true
	case "read":
		return LevelRead, true
	case "operate":
		return LevelOperate, true
	}
	return LevelNone, false
}

// Workspace loads the workspace by name if actor has at least need on it: ErrNotFound if there is
// none or the actor can't see it, ErrForbidden if the actor's level is below need.
func (a *App) Workspace(ctx context.Context, actor Actor, name string, need Level) (storage.Workspace, Level, error) {
	ws, err := a.Store.GetWorkspace(ctx, name)
	if err != nil {
		return ws, LevelNone, err
	}
	return a.access(ctx, actor, ws, need)
}

// WorkspaceByID is Workspace by id.
func (a *App) WorkspaceByID(ctx context.Context, actor Actor, id uuid.UUID, need Level) (storage.Workspace, Level, error) {
	ws, err := a.Store.GetWorkspaceByID(ctx, id)
	if err != nil {
		return ws, LevelNone, err
	}
	return a.access(ctx, actor, ws, need)
}

// access is the one access rule: see Workspace.
func (a *App) access(ctx context.Context, actor Actor, ws storage.Workspace, need Level) (storage.Workspace, Level, error) {
	level := LevelOperate
	if !actor.Admin {
		s, err := a.Store.Access(ctx, ws.ID, actor.UserID)
		if err != nil {
			return storage.Workspace{}, LevelNone, err
		}
		level, _ = ParseLevel(s)
	}
	switch {
	case level == LevelNone:
		return storage.Workspace{}, LevelNone, ErrNotFound
	case level < need:
		return ws, level, errReadOnly
	}
	return ws, level, nil
}
