package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// PollVCSRuns queues the runs VCS events call for, once (a scheduler job): an apply run for a new
// commit on a workspace's branch, a speculative plan for each new commit of a pull request into it.
// Polling, like PollVCSModules, needs no inbound webhooks, so Opsolete can stay internal. Each
// repository branch is polled once per round however many workspaces link it, and on GitHub with
// conditional requests, so an unchanged branch costs no rate limit.
func (a *App) PollVCSRuns(ctx context.Context) error {
	wss, err := a.Store.VCSTriggered(ctx)
	if err != nil {
		return fmt.Errorf("list vcs-triggered workspaces: %w", err)
	}
	groups := map[string][]storage.Workspace{}
	for _, ws := range wss {
		k := ws.VCSConnectionID.String() + "\n" + strings.ToLower(ws.VCSRepo) + "\n" + ws.VCSBranch
		groups[k] = append(groups[k], ws)
	}
	for _, g := range groups {
		if err := a.TriggerVCSRuns(ctx, g); err != nil && ctx.Err() == nil {
			a.Log.Warn("vcs trigger", "repo", g[0].VCSRepo, "branch", g[0].VCSBranch, "workspaces", len(g), "err", err)
		}
	}
	return nil
}

// TriggerVCSRuns queues the runs a branch and its pull requests call for (see PollVCSRuns) on the
// workspaces of group, which all link the same connection, repository and branch. A workspace with
// an active run gets them at a later round, one at a time. One workspace's error (or a failed list
// of tags, head or pull requests) doesn't keep the others from their runs; all are returned.
func (a *App) TriggerVCSRuns(ctx context.Context, group []storage.Workspace) error {
	ws := group[0]
	sc, err := a.Store.GetVCSConnection(ctx, ws.VCSConnectionID)
	if err != nil {
		return err
	}
	c, err := a.conn(sc)
	if err != nil {
		return err
	}
	if err := c.Paused(); err != nil {
		return err
	}
	// each list is fetched once per round, at first need; a failed fetch fails just its users
	tags := sync.OnceValues(func() ([]vcs.Tag, error) { return c.Tags(ctx, ws.VCSRepo) })
	head := sync.OnceValues(func() (string, error) { return c.Head(ctx, ws.VCSRepo, ws.VCSBranch) })
	prs := sync.OnceValues(func() ([]vcs.PullRequest, error) { return c.PullRequests(ctx, ws.VCSRepo, ws.VCSBranch) })
	var errs []error
	queue := func(ws storage.Workspace, commit string, pr int, msg string) {
		if _, err := a.Store.QueueVCSRun(ctx, ws.ID, commit, pr, msg); err != nil {
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
			} else if tag, ok := latestTag(list, ws.VCSTagsRegex); ok && tag.Commit != ws.VCSTagBaseline {
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

// latestTag returns the matching tag with the highest version in its name; tag-based workspaces
// run it. A bad pattern matches nothing.
// shortcut: tags without a version number (1.2 or 1.2.3) are never newest; order by date if custom patterns need it.
func latestTag(tags []vcs.Tag, pattern string) (vcs.Tag, bool) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return vcs.Tag{}, false
	}
	var best vcs.Tag
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
