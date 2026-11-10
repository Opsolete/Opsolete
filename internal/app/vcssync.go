package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// noFolderCache remembers, per linked module (connection, repo, folder), the tag commits whose
// archive lacked the module's folder (vcs.ErrNoFolder). It lives on this replica.
type noFolderCache struct {
	mu sync.Mutex
	m  map[string]map[string]error // module key -> tag commit -> the ErrNoFolder Archive gave there
}

// syncTags publishes every semver tag of a linked repo that isn't a module version yet, and records
// the outcome. A bad tag doesn't stop the others. It returns the versions it added.
// shortcut: a tag that fails to publish is retried on every sync; fine for the odd broken tag.
func (a *App) syncTags(ctx context.Context, m storage.VCSModule) ([]string, error) {
	added, err := a.publishTags(ctx, m)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if serr := a.Store.SetVCSSync(ctx, m.Namespace, m.Name, m.System, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	return added, err
}

func (a *App) publishTags(ctx context.Context, m storage.VCSModule) ([]string, error) {
	st := a.Store
	sc, err := st.GetVCSConnection(ctx, m.ConnectionID)
	if err != nil {
		return nil, err
	}
	c, err := a.conn(sc)
	if err != nil {
		return nil, err
	}
	if err := c.Paused(); err != nil {
		return nil, err
	}
	tags, err := c.Tags(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	existing, err := st.ModuleVersions(ctx, m.Namespace, m.Name, m.System)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, v := range existing {
		have[v] = true
	}
	// a commit's tree never changes: once without the folder, always without it, so a sync skips the download.
	// Only unpublished tags still listed are kept: the cache is pruned on every sync.
	mod, nf := noFolderKey(m), &a.noFolder
	nf.mu.Lock()
	seen := nf.m[mod]
	nf.mu.Unlock()
	kept := map[string]error{}
	var added []string
	var errs, noFolder []error
	for _, t := range tags {
		tag := t.Name
		rest, ok := strings.CutPrefix(tag, m.TagPrefix)
		ver, err := registry.CanonicalVersion(rest)
		if !ok || err != nil || have[ver] {
			continue // not this module's release tag, or already published (v1.0.0 and 1.0.0 are one version)
		}
		have[ver] = true
		if known := seen[t.Commit]; known != nil && t.Commit != "" {
			kept[t.Commit] = known
			noFolder = append(noFolder, fmt.Errorf("tag %s: %w", tag, known))
			continue
		}
		archive, err := moduleArchive(ctx, c, m.Repo, tag, "tag", m.Dir)
		if errors.Is(err, vcs.ErrNoFolder) {
			if t.Commit != "" {
				kept[t.Commit] = err
			}
			noFolder = append(noFolder, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		ins, err := registry.Inspect(archive)
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		err = st.CreateModule(ctx, storage.Module{Namespace: m.Namespace, Name: m.Name, System: m.System, Version: ver, Variables: ins.Variables},
			storage.ModuleDocs{Module: ins.Module, Readme: ins.Readme}, archive)
		if errors.Is(err, storage.ErrExists) {
			continue // another replica was faster
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tag %s: %w", tag, err))
			continue
		}
		added = append(added, ver)
	}
	nf.mu.Lock()
	if len(kept) > 0 {
		if nf.m == nil {
			nf.m = map[string]map[string]error{}
		}
		nf.m[mod] = kept
	} else {
		delete(nf.m, mod)
	}
	nf.mu.Unlock()
	// in a monorepo, tags from before the module's folder existed aren't its versions; only a folder no tag has is an error
	if len(existing) == 0 && len(added) == 0 {
		errs = append(errs, noFolder...)
	}
	return added, errors.Join(errs...)
}

// PollVCSModules syncs every linked module once (a scheduler job). It needs no inbound webhooks, so
// Opsolete can stay private. A module's error is logged and doesn't stop the others.
func (a *App) PollVCSModules(ctx context.Context) error {
	mods, err := a.Store.VCSModules(ctx)
	if err != nil {
		return fmt.Errorf("list vcs modules: %w", err)
	}
	linked := map[string]bool{}
	for _, m := range mods {
		linked[noFolderKey(m)] = true
	}
	a.noFolder.mu.Lock()
	for k := range a.noFolder.m { // unlinked (or relinked to another folder) since
		if !linked[k] {
			delete(a.noFolder.m, k)
		}
	}
	a.noFolder.mu.Unlock()
	for _, m := range mods {
		added, err := a.syncTags(ctx, m)
		if err != nil && ctx.Err() == nil {
			a.Log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "repo", m.Repo, "err", err)
		}
		if len(added) > 0 {
			a.Log.Info("module versions published from vcs", "module", m.Namespace+"/"+m.Name+"/"+m.System, "versions", added)
		}
	}
	return nil
}

func noFolderKey(m storage.VCSModule) string {
	return m.ConnectionID.String() + "\x00" + m.Repo + "\x00" + m.Dir
}
