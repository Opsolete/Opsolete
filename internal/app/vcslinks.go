package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// A workspace's own configuration can be a VCS branch (or its tags) of a repository that one of the
// connections it may use (storage.VCSConnection.Allows) can read. Linking needs operate access.

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// ValidBranch: a branch name a link or the template catalog may follow.
func ValidBranch(b string) bool { return branchRe.MatchString(b) && !strings.Contains(b, "..") }

// TagFormats are the tag-based trigger formats offered, as the patterns they stand for.
var TagFormats = map[string]string{"semantic": `^\d+\.\d+\.\d+$`, "prefix": `\d+\.\d+\.\d+$`, "suffix": `^\d+\.\d+\.\d+`}

// VCSLinkInput is a workspace's repository link as a form gives it.
type VCSLinkInput struct {
	ConnectionID          uuid.UUID // uuid.Nil: no link (unlinks)
	Repo, Branch          string    // Branch "" is main
	Tags                  bool      // tag-based triggers: TagFormat (a TagFormats key), else the pattern TagsRegex
	TagFormat, TagsRegex  string
	Trigger, PullRequests bool // pushes queue runs; pull requests get speculative plans (not tag-based)
}

// resolveVCSLink checks a link for ws (for a new workspace: Name "" and its ProjectName): a branch
// name or tag pattern, a connection ws may use, and that its branch or tags can be read. A changed
// tag-based link gets its baseline: the newest matching tag, which never runs (only tags pushed later
// do). nil: no link. ValidationError: anything of the link.
func (a *App) resolveVCSLink(ctx context.Context, ws storage.Workspace, in VCSLinkInput) (*storage.VCSLink, error) {
	if in.ConnectionID == uuid.Nil {
		return nil, nil
	}
	if !a.VCSEnabled {
		return nil, invalid("vcs_connection", "VCS connections are disabled")
	}
	l := storage.VCSLink{ConnectionID: in.ConnectionID, Repo: strings.TrimSpace(in.Repo), Branch: strings.TrimSpace(in.Branch)}
	if l.Branch == "" {
		l.Branch = "main"
	}
	if !ValidBranch(l.Branch) {
		return nil, invalid("vcs_branch", fmt.Sprintf("%q is not a branch name", l.Branch))
	}
	if in.Tags {
		if l.TagsRegex = TagFormats[in.TagFormat]; l.TagsRegex == "" {
			l.TagsRegex = strings.TrimSpace(in.TagsRegex)
			if _, err := regexp.Compile(l.TagsRegex); err != nil || l.TagsRegex == "" || len(l.TagsRegex) > 255 {
				return nil, invalid("vcs_tags_regex", fmt.Sprintf("%q is not a tag pattern: use a regular expression such as ^v\\d+\\.\\d+\\.\\d+$", l.TagsRegex))
			}
		}
	}
	// tag-based: tags always trigger, and pull requests get no plans
	l.Trigger, l.PullRequests = in.Trigger || l.TagsRegex != "", in.PullRequests && l.TagsRegex == ""
	c, err := a.vcsConn(ctx, l.ConnectionID, ws.Name, ws.ProjectName)
	if err != nil {
		return nil, err
	}
	if l.TagsRegex == "" {
		if _, err := c.Head(ctx, l.Repo, l.Branch); err != nil {
			return nil, invalid("vcs_branch", fmt.Sprintf("can't read branch %s of %q: %v", l.Branch, l.Repo, err))
		}
		return &l, nil
	}
	tags, err := c.Tags(ctx, l.Repo)
	if err != nil {
		return nil, invalid("vcs_repo", fmt.Sprintf("can't read the tags of %q: %v", l.Repo, err))
	}
	// storage keeps the baseline of an unchanged link (connection, repository, tag format)
	if l.ConnectionID != ws.VCSConnectionID || l.Repo != ws.VCSRepo || l.TagsRegex != ws.VCSTagsRegex {
		t, _ := vcs.LatestTag(tags, l.TagsRegex)
		l.TagBaseline = t.Commit
	}
	return &l, nil
}

// LinkVCS links a workspace to a repository branch or its tags, changes its triggers, or unlinks it
// (in.ConnectionID uuid.Nil). Operate access; ValidationError: the link (see resolveVCSLink);
// storage.ErrRunActive (a conflict) during an active run: its apply must fetch the commit its plan
// used. A changed link detaches the workspace's variable sets (storage.SetWorkspaceVCS).
func (a *App) LinkVCS(ctx context.Context, actor Actor, workspace string, in VCSLinkInput) (storage.Workspace, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return ws, err
	}
	l, err := a.resolveVCSLink(ctx, ws, in)
	if err != nil {
		return ws, err
	}
	if l == nil {
		l = &storage.VCSLink{}
	}
	if err := a.Store.SetWorkspaceVCS(ctx, ws.ID, l.ConnectionID, l.Repo, l.Branch, l.TagsRegex, l.TagBaseline, l.Trigger, l.PullRequests); err != nil {
		return ws, err
	}
	a.Log.Info("workspace vcs", "workspace", ws.Name, "repo", l.Repo, "branch", l.Branch,
		"trigger", l.Trigger, "tags", l.TagsRegex, "pull_requests", l.PullRequests, "user", actor.Email)
	return ws, nil
}

// LinkableVCS lists the connections a workspace may link and, if reposOf is one of them, its
// repositories (a failed listing is logged: typing the repository still works). workspace "" is a new
// workspace in project (by name; "" the default), which actor must be able to create; else actor
// needs operate access to workspace, and project is ignored. With VCS off the list is empty.
func (a *App) LinkableVCS(ctx context.Context, actor Actor, workspace, project string, reposOf uuid.UUID) ([]storage.VCSConnection, []string, error) {
	if workspace == "" {
		p, err := a.NewWorkspaceProject(ctx, actor, project, uuid.Nil)
		if err != nil {
			return nil, nil, err
		}
		project = p.Name
	} else {
		ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
		if err != nil {
			return nil, nil, err
		}
		project = ws.ProjectName
	}
	if !a.VCSEnabled {
		return nil, nil, nil
	}
	conns, err := a.Store.VCSConnections(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out []storage.VCSConnection
	var repos []string
	for _, sc := range conns {
		if !sc.Allows(workspace, project) {
			continue
		}
		out = append(out, sc)
		if sc.ID != reposOf {
			continue
		}
		c, err := a.VCS.ConnFor(a.OpenToken, sc)
		if err == nil {
			repos, err = c.Repos(ctx)
		}
		if err != nil {
			a.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
		}
	}
	return out, repos, nil
}

// VCSHealth checks that a linked workspace can read its source as its runs do, for the "Connected to
// VCS" mark: its connection and, if the workspace may use it, its branch or tags. It returns the
// connection (nil if it's gone) and the problem ("" if none). Operate access.
func (a *App) VCSHealth(ctx context.Context, actor Actor, workspace string) (*storage.VCSConnection, string, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return nil, "", err
	}
	sc, err := a.Store.GetVCSConnection(ctx, ws.VCSConnectionID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, "its VCS connection was deleted", nil
	}
	if err != nil {
		return nil, "", err
	}
	if !sc.Allows(ws.Name, ws.ProjectName) {
		return &sc, "an admin no longer allows this workspace to use " + sc.Name + ": its runs fail. Change source, or ask an admin to attach it under VCS providers.", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := a.VCS.ConnFor(a.OpenToken, sc)
	if err == nil && ws.VCSTagsRegex != "" {
		_, err = c.Tags(ctx, ws.VCSRepo)
	} else if err == nil {
		_, err = c.Head(ctx, ws.VCSRepo, ws.VCSBranch)
	}
	if err != nil {
		return &sc, err.Error(), nil
	}
	return &sc, "", nil
}
