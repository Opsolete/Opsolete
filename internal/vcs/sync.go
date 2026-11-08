package vcs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
)

// Sync publishes every semver tag of a linked repo that isn't a module version yet, and records
// the outcome. A bad tag doesn't stop the others. It returns the versions it added.
// shortcut: a tag that fails to publish is retried on every sync; fine for the odd broken tag.
func (cl *Client) Sync(ctx context.Context, st *storage.Store, open OpenToken, m storage.VCSModule) ([]string, error) {
	added, err := cl.syncModule(ctx, st, open, m)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if serr := st.SetVCSSync(ctx, m.Namespace, m.Name, m.System, msg); serr != nil {
		err = errors.Join(err, serr)
	}
	return added, err
}

func (cl *Client) syncModule(ctx context.Context, st *storage.Store, open OpenToken, m storage.VCSModule) ([]string, error) {
	sc, err := st.GetVCSConnection(ctx, m.ConnectionID)
	if err != nil {
		return nil, err
	}
	c, err := cl.ConnFor(open, sc)
	if err != nil {
		return nil, err
	}
	if err := c.paused(); err != nil {
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
	// a commit's tree never changes: once without the folder, always without it, so Sync skips the download.
	// Only unpublished tags still listed are kept: the cache is pruned on every sync.
	mod := noFolderKey(m)
	cl.mu.Lock()
	seen := cl.noFolder[mod]
	cl.mu.Unlock()
	kept := map[string]error{}
	var added []string
	var errs, noFolder []error
	for _, t := range tags {
		tag := t.Name
		ver, err := registry.CanonicalVersion(tag)
		if err != nil || have[ver] {
			continue // not a release tag, or already published (v1.0.0 and 1.0.0 are one version)
		}
		have[ver] = true
		if known := seen[t.Commit]; known != nil && t.Commit != "" {
			kept[t.Commit] = known
			noFolder = append(noFolder, fmt.Errorf("tag %s: %w", tag, known))
			continue
		}
		archive, err := c.Archive(ctx, m.Repo, tag, "tag", m.Dir)
		if errors.Is(err, ErrNoFolder) {
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
	cl.mu.Lock()
	if len(kept) > 0 {
		cl.noFolder[mod] = kept
	} else {
		delete(cl.noFolder, mod)
	}
	cl.mu.Unlock()
	// in a monorepo, tags from before the module's folder existed aren't its versions; only a folder no tag has is an error
	if len(existing) == 0 && len(added) == 0 {
		errs = append(errs, noFolder...)
	}
	return added, errors.Join(errs...)
}

// Poll syncs every linked module once (a scheduler job). It needs no inbound webhooks, so Opsolete
// can stay private. A module's error is logged and doesn't stop the others.
func (cl *Client) Poll(ctx context.Context, st *storage.Store, open OpenToken, log *slog.Logger) error {
	mods, err := st.VCSModules(ctx)
	if err != nil {
		return fmt.Errorf("list vcs modules: %w", err)
	}
	linked := map[string]bool{}
	for _, m := range mods {
		linked[noFolderKey(m)] = true
	}
	cl.mu.Lock()
	for k := range cl.noFolder { // unlinked (or relinked to another folder) since
		if !linked[k] {
			delete(cl.noFolder, k)
		}
	}
	cl.mu.Unlock()
	for _, m := range mods {
		added, err := cl.Sync(ctx, st, open, m)
		if err != nil && ctx.Err() == nil {
			log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "repo", m.Repo, "err", err)
		}
		if len(added) > 0 {
			log.Info("module versions published from vcs", "module", m.Namespace+"/"+m.Name+"/"+m.System, "versions", added)
		}
	}
	return nil
}

func noFolderKey(m storage.VCSModule) string {
	return m.ConnectionID.String() + "\x00" + m.Repo + "\x00" + m.Dir
}

// PollRuns queues the runs VCS events call for, once (a scheduler job): an apply run for a new
// commit on a workspace's branch, a speculative plan for each new commit of a pull request into it.
// Polling, like Poll, needs no inbound webhooks, so Opsolete can stay internal. Each repository
// branch is polled once per round however many workspaces link it, and on GitHub with conditional
// requests, so an unchanged branch costs no rate limit.
func (cl *Client) PollRuns(ctx context.Context, st *storage.Store, open OpenToken, log *slog.Logger) error {
	wss, err := st.VCSTriggered(ctx)
	if err != nil {
		return fmt.Errorf("list vcs-triggered workspaces: %w", err)
	}
	groups := map[string][]storage.Workspace{}
	for _, ws := range wss {
		k := ws.VCSConnectionID.String() + "\n" + strings.ToLower(ws.VCSRepo) + "\n" + ws.VCSBranch
		groups[k] = append(groups[k], ws)
	}
	for _, g := range groups {
		if err := cl.TriggerRuns(ctx, st, open, g); err != nil && ctx.Err() == nil {
			log.Warn("vcs trigger", "repo", g[0].VCSRepo, "branch", g[0].VCSBranch, "workspaces", len(g), "err", err)
		}
	}
	return nil
}

// TriggerRuns queues the runs a branch and its pull requests call for (see PollRuns) on the
// workspaces of group, which all link the same connection, repository and branch. A workspace with
// an active run gets them at a later round, one at a time. One workspace's error (or a failed list
// of tags, head or pull requests) doesn't keep the others from their runs; all are returned.
func (cl *Client) TriggerRuns(ctx context.Context, st *storage.Store, open OpenToken, group []storage.Workspace) error {
	ws := group[0]
	sc, err := st.GetVCSConnection(ctx, ws.VCSConnectionID)
	if err != nil {
		return err
	}
	c, err := cl.ConnFor(open, sc)
	if err != nil {
		return err
	}
	if err := c.paused(); err != nil {
		return err
	}
	// each list is fetched once per round, at first need; a failed fetch fails just its users
	tags := sync.OnceValues(func() ([]Tag, error) { return c.Tags(ctx, ws.VCSRepo) })
	head := sync.OnceValues(func() (string, error) { return c.Head(ctx, ws.VCSRepo, ws.VCSBranch) })
	prs := sync.OnceValues(func() ([]PullRequest, error) { return c.PullRequests(ctx, ws.VCSRepo, ws.VCSBranch) })
	var errs []error
	queue := func(ws storage.Workspace, commit string, pr int, msg string) {
		if _, err := st.QueueVCSRun(ctx, ws.ID, commit, pr, msg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ws.Name, err))
		}
	}
	for _, ws := range group {
		if !sc.Allows(ws.Name, ws.ProjectName) { // deselected (or moved out of a selected project) since the link; manual runs fail with the reason
			continue
		}
		if ws.VCSTagsRegex != "" { // tag-based: no branch commits, no pull requests
			list, err := tags()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: tags: %w", ws.Name, err))
			} else if tag, ok := LatestTag(list, ws.VCSTagsRegex); ok && tag.Commit != ws.VCSTagBaseline {
				queue(ws, tag.Commit, 0, fmt.Sprintf("tag %s at %s: apply plan queued, waiting for a runner", tag.Name, short(tag.Commit)))
			}
			continue
		}
		if ws.VCSTrigger {
			if h, err := head(); err != nil {
				errs = append(errs, fmt.Errorf("%s: branch head: %w", ws.Name, err))
			} else {
				queue(ws, h, 0, fmt.Sprintf("commit %s on %s: apply plan queued, waiting for a runner", short(h), ws.VCSBranch))
			}
		}
		if !ws.VCSPullRequests {
			continue
		}
		list, err := prs()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: pull requests: %w", ws.Name, err))
			continue
		}
		for _, pr := range list {
			queue(ws, pr.Commit, pr.Number, fmt.Sprintf("pull request #%d at %s: speculative plan queued, waiting for a runner", pr.Number, short(pr.Commit)))
		}
	}
	return errors.Join(errs...)
}

func short(commit string) string { return commit[:min(len(commit), 8)] }

var tagVersionRe = regexp.MustCompile(`\d+(\.\d+)+(-[0-9A-Za-z.-]+)?`) // keeps -rc: a prerelease ranks below its release

// LatestTag returns the matching tag with the highest version in its name; tag-based workspaces
// run it. A bad pattern matches nothing.
// shortcut: tags without a version number (1.2 or 1.2.3) are never newest; order by date if custom patterns need it.
func LatestTag(tags []Tag, pattern string) (Tag, bool) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Tag{}, false
	}
	var best Tag
	var bestV *version.Version
	for _, t := range tags {
		if !re.MatchString(t.Name) || t.Commit == "" {
			continue
		}
		v, err := version.NewVersion(tagVersionRe.FindString(t.Name))
		if err == nil && (bestV == nil || v.GreaterThan(bestV)) {
			best, bestV = t, v
		}
	}
	return best, bestV != nil
}
