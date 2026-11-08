package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// ErrVCSFetch: the provider didn't serve a run's commit (or the workspace may no longer use its
// connection); the message says why. HTTP answers it 502.
var ErrVCSFetch = errors.New("fetching the repository failed")

var errArchiveLink = reason{ErrForbidden, "invalid or expired download link"}

// vcsConn opens a connection the workspace (by name, "" = a new one) in project (by name) may link
// (storage.VCSConnection.Allows, the rule the run poller applies too). A connection that is gone or
// doesn't allow the workspace is a ValidationError on vcs_connection.
func (a *App) vcsConn(ctx context.Context, connID uuid.UUID, workspace, project string) (vcs.Conn, error) {
	sc, err := a.Store.GetVCSConnection(ctx, connID)
	if errors.Is(err, storage.ErrNotFound) {
		return vcs.Conn{}, invalid("vcs_connection", "the VCS connection no longer exists")
	}
	if err != nil {
		return vcs.Conn{}, err
	}
	if !sc.Allows(workspace, project) {
		if workspace == "" {
			return vcs.Conn{}, invalid("vcs_connection", fmt.Sprintf("new workspaces in project %s may not link repositories through VCS connection %s: ask an admin", project, sc.Name))
		}
		return vcs.Conn{}, invalid("vcs_connection", fmt.Sprintf("workspace %q may not link repositories through VCS connection %s: ask an admin", workspace, sc.Name))
	}
	return vcs.ConnFor(a.SecretKey, sc)
}

// pinCommit records the commit a plan job runs, unless the run was queued with its commit: the head
// of the workspace's branch, or for tag-based workspaces the newest matching tag.
func (a *App) pinCommit(ctx context.Context, j storage.Job) error {
	ws, err := a.Store.GetWorkspaceByID(ctx, j.WorkspaceID)
	if err != nil {
		return err
	}
	c, err := a.vcsConn(ctx, ws.VCSConnectionID, ws.Name, ws.ProjectName) // fails if the admin deselected the workspace since the link
	if err != nil {
		return fmt.Errorf("vcs: %w", err)
	}
	if j.VCSCommit != "" { // queued for a pushed commit or pull request
		return nil
	}
	var head string
	if ws.VCSTagsRegex != "" {
		tags, err := c.Tags(ctx, ws.VCSRepo)
		if err != nil {
			return fmt.Errorf("vcs: %w", err)
		}
		tag, ok := vcs.LatestTag(tags, ws.VCSTagsRegex)
		if !ok {
			return fmt.Errorf("vcs: no tag of %s matches %s", ws.VCSRepo, ws.VCSTagsRegex)
		}
		head = tag.Commit
	} else if head, err = c.Head(ctx, ws.VCSRepo, ws.VCSBranch); err != nil {
		return fmt.Errorf("vcs: %w", err)
	}
	_, err = a.Store.PinVCSCommit(ctx, j.RunID, head)
	return err
}

// VCSArchivePath is where a run's pinned commit is served (GET /v1/vcs-archives/{file}); it ends in
// .tar.gz so Terraform's getter unpacks it. Under /v1/, it's public and authorised by its signature.
func VCSArchivePath(runID uuid.UUID) string { return "/v1/vcs-archives/" + runID.String() + ".tar.gz" }

// VCSArchive is a run's pinned commit of its workspace's repository as a .tar.gz, for the runner
// that got the signed URL (expires, sig) of VCSArchivePath(runID) with its job. ErrForbidden: the
// signature is wrong, expired or for another path; ErrNotFound: no such run, or no pinned commit;
// ErrVCSFetch: the provider failed or the workspace may no longer use its connection.
func (a *App) VCSArchive(ctx context.Context, runID uuid.UUID, expires, sig string) ([]byte, error) {
	if !a.Registry.VerifyArchiveURL(VCSArchivePath(runID), expires, sig) {
		return nil, errArchiveLink
	}
	run, _, err := a.Store.GetRun(ctx, runID)
	if err == nil && run.VCSCommit == "" {
		err = ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ws, err := a.Store.GetWorkspaceByID(ctx, run.WorkspaceID)
	if err != nil {
		return nil, err
	}
	c, err := a.vcsConn(ctx, ws.VCSConnectionID, ws.Name, ws.ProjectName)
	var b []byte
	if err == nil {
		b, err = c.Archive(ctx, ws.VCSRepo, run.VCSCommit, "commit", "")
	}
	if err != nil {
		a.Log.Warn("vcs archive", "run", run.ID, "repo", ws.VCSRepo, "err", err)
		return nil, fmt.Errorf("%w: %w", ErrVCSFetch, err)
	}
	return b, nil
}
