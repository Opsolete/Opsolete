package app

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// Registry modules can publish from a repository: its semver tags become versions (vcs.Client.Sync), now
// when linked or asked, and every few minutes by the poller (vcs.Client.Poll). Linking, syncing and
// unlinking are admins only; a sync error is recorded on the module and shown with it.

var dirRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,255}$`)

// RepoDir cleans a folder of a repository ("" = its root); ok is false if it isn't one.
func RepoDir(raw string) (dir string, ok bool) {
	if dir = strings.Trim(strings.TrimSpace(raw), "/"); dir != "" {
		dir = path.Clean(dir)
	}
	if dir == "." {
		dir = ""
	}
	return dir, dirRe.MatchString(dir) && dir != ".." && !strings.HasPrefix(dir, "../")
}

// Terraform's module repository naming convention: terraform-<system>-<name>.
var tfRepoRe = regexp.MustCompile(`^terraform-([a-z0-9]+)-([a-z0-9][a-z0-9_-]*)$`)

// RepoModuleAddress is the module address a repository suggests: the namespace is its name, name
// and system follow the naming convention ("" if it doesn't).
func RepoModuleAddress(repo string) (namespace, name, system string) {
	base := strings.ToLower(path.Base(repo))
	if m := tfRepoRe.FindStringSubmatch(base); m != nil {
		name, system = m[2], m[1]
	}
	if repo == "" {
		base = ""
	}
	return base, name, system
}

// NewVCSModule links a repository (a folder of it) to a registry module.
type NewVCSModule struct {
	ConnectionID            uuid.UUID
	Repo, Dir               string
	Namespace, Name, System string // "" follows the repository (RepoModuleAddress)
}

// PublishVCSModule links a repository to a registry module and publishes its tags right away (see
// SyncVCSModule). Admins only; ErrNotFound: no such connection; ValidationError: the address, repository or folder, or the module is
// already published from a repository.
func (a *App) PublishVCSModule(ctx context.Context, actor Actor, in NewVCSModule) (storage.VCSModule, []string, error) {
	sc, err := a.VCSConnection(ctx, actor, in.ConnectionID)
	if err != nil {
		return storage.VCSModule{}, nil, err
	}
	in.Repo = strings.TrimSpace(in.Repo)
	ns, name, sys := RepoModuleAddress(in.Repo)
	for _, f := range []struct {
		v *string
		d string
	}{{&in.Namespace, ns}, {&in.Name, name}, {&in.System, sys}} {
		if *f.v = strings.TrimSpace(*f.v); *f.v == "" {
			*f.v = f.d
		}
	}
	ns, name, sys, _, err = a.Registry.Address(in.Namespace, in.Name, in.System)
	if err != nil {
		return storage.VCSModule{}, nil, &ValidationError{Message: err.Error()}
	}
	if !strings.Contains(in.Repo, "/") {
		return storage.VCSModule{}, nil, invalid("repo", "choose a repository")
	}
	dir, ok := RepoDir(in.Dir)
	if !ok {
		return storage.VCSModule{}, nil, invalid("dir", fmt.Sprintf("%q is not a folder of the repository", in.Dir))
	}
	m := storage.VCSModule{Namespace: ns, Name: name, System: sys, ConnectionID: sc.ID, Repo: in.Repo, Dir: dir}
	if err := a.Store.LinkVCSModule(ctx, m); errors.Is(err, storage.ErrExists) {
		return m, nil, &ValidationError{Message: fmt.Sprintf("%s/%s/%s is already published from a repository", ns, name, sys)}
	} else if err != nil {
		return m, nil, err
	}
	a.Log.Info("module linked to vcs", "module", ns+"/"+name+"/"+sys, "repo", m.Repo, "dir", m.Dir, "user", actor.Email)
	return m, a.syncVCSModule(ctx, m), nil
}

// SyncVCSModule publishes the module's new tags now and returns the versions it added. A sync error
// isn't returned: it is recorded on the module (VCSModule.SyncError). Admins only; ErrNotFound: the
// module isn't published from a repository.
func (a *App) SyncVCSModule(ctx context.Context, actor Actor, namespace, name, system string) (storage.VCSModule, []string, error) {
	if !actor.Admin {
		return storage.VCSModule{}, nil, errAdminsOnly
	}
	m, err := a.Store.GetVCSModule(ctx, namespace, name, system)
	if err != nil {
		return m, nil, err
	}
	return m, a.syncVCSModule(ctx, m), nil
}

func (a *App) syncVCSModule(ctx context.Context, m storage.VCSModule) []string {
	added, err := a.VCS.Sync(ctx, a.Store, a.OpenToken, m)
	if err != nil {
		a.Log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "err", err)
	}
	return added
}

// UnlinkVCSModule stops publishing a module from its repository; its published versions stay.
// Changing the repository or folder is unlinking and publishing again. Admins only; ErrNotFound: not linked.
func (a *App) UnlinkVCSModule(ctx context.Context, actor Actor, namespace, name, system string) error {
	if !actor.Admin {
		return errAdminsOnly
	}
	if err := a.Store.UnlinkVCSModule(ctx, namespace, name, system); err != nil {
		return err
	}
	a.Log.Info("module unlinked from vcs", "module", namespace+"/"+name+"/"+system, "user", actor.Email)
	return nil
}

// VCSModules lists the modules published from repositories, with their sync errors. Admins only.
func (a *App) VCSModules(ctx context.Context, actor Actor) ([]storage.VCSModule, error) {
	if !actor.Admin {
		return nil, errAdminsOnly
	}
	return a.Store.VCSModules(ctx)
}

// VCSModule is the repository a module publishes from, with its sync error, for the module's page
// (everyone who sees the registry sees it); nil if it isn't linked.
func (a *App) VCSModule(ctx context.Context, namespace, name, system string) (*storage.VCSModule, error) {
	m, err := a.Store.GetVCSModule(ctx, namespace, name, system)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	return &m, err
}
