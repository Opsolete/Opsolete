package httpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Workspaces without a template: empty, or holding only variables,
// or taking their configuration from a VCS branch. Runs need configuration (a template or a branch).

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// vcsForm is the repository part of the new-workspace and link forms.
type vcsForm struct {
	Conns []storage.VCSConnection
	Repos []string // of all connections, for the repo dropdown
	Form  map[string]string
	Error string
}

// loadVCSForm lists the connections the workspace (by name, "" = a new one) may link and the repos
// of reposOf ("*" = all connections, "" = none); with VCS off it's empty and the form hides the part.
func (s *Server) loadVCSForm(ctx context.Context, form map[string]string, errMsg, workspace, reposOf string) (vcsForm, error) {
	f := vcsForm{Form: form, Error: errMsg}
	if !s.Features.VCS {
		return f, nil
	}
	conns, err := s.Store.VCSConnections(ctx)
	if err != nil {
		return f, err
	}
	for _, sc := range conns {
		if !sc.AllowsWorkspace(workspace) {
			continue
		}
		f.Conns = append(f.Conns, sc)
		if reposOf != "*" && reposOf != sc.ID {
			continue
		}
		c, err := vcs.ConnFor(s.SecretKey, sc)
		if err == nil {
			var repos []string
			if repos, err = c.Repos(ctx); err == nil {
				f.Repos = append(f.Repos, repos...)
			}
		}
		if err != nil { // typing the repo still works
			s.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
		}
	}
	return f, nil
}

func vcsFormValues(r *http.Request) map[string]string {
	form := map[string]string{}
	for _, k := range []string{"type", "name", "auto_apply", "vcs_connection", "vcs_repo", "vcs_branch", "vcs_trigger", "vcs_pull_requests",
		"vcs_trigger_type", "vcs_tag_format", "vcs_tags_regex"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	return form
}

// tagFormats are the tag-based trigger formats HCP Terraform offers, as the patterns they stand for.
var tagFormats = map[string]string{"semantic": `^\d+\.\d+\.\d+$`, "prefix": `\d+\.\d+\.\d+$`, "suffix": `^\d+\.\d+\.\d+`}

// fillTagForm sets the form's trigger type and tag format from a workspace's tag pattern.
func fillTagForm(form map[string]string, tagsRegex string) {
	form["vcs_trigger_type"], form["vcs_tag_format"], form["vcs_tags_regex"] = "branch", "semantic", ""
	if tagsRegex == "" {
		return
	}
	form["vcs_trigger_type"], form["vcs_tag_format"], form["vcs_tags_regex"] = "tag", "custom", tagsRegex
	for k, re := range tagFormats {
		if re == tagsRegex {
			form["vcs_tag_format"], form["vcs_tags_regex"] = k, ""
		}
	}
}

// vcsLink validates the form's repository link for the workspace (by name) and checks the branch
// exists, or for tag-based triggers (tags: their pattern) that the tags can be read; no connection = no link.
func (s *Server) vcsLink(ctx context.Context, workspace string, form map[string]string) (connID, repo, branch, tags string, err error) {
	connID, repo, branch = form["vcs_connection"], form["vcs_repo"], form["vcs_branch"]
	if connID == "" {
		return "", "", "", "", nil
	}
	if !s.Features.VCS {
		return "", "", "", "", errors.New("VCS connections are disabled")
	}
	if branch == "" {
		branch = "main"
	}
	if !branchRe.MatchString(branch) || strings.Contains(branch, "..") {
		return "", "", "", "", fmt.Errorf("%q is not a branch name", branch)
	}
	if form["vcs_trigger_type"] == "tag" {
		if tags = tagFormats[form["vcs_tag_format"]]; tags == "" {
			tags = strings.TrimSpace(form["vcs_tags_regex"])
			if _, err := regexp.Compile(tags); err != nil || tags == "" || len(tags) > 255 {
				return "", "", "", "", fmt.Errorf("%q is not a tag pattern: use a regular expression such as ^v\\d+\\.\\d+\\.\\d+$", tags)
			}
		}
	}
	c, err := s.vcsConn(ctx, connID, workspace)
	if err != nil {
		return "", "", "", "", err
	}
	if tags != "" {
		if _, err := c.Tags(ctx, repo); err != nil {
			return "", "", "", "", fmt.Errorf("can't read the tags of %q: %w", repo, err)
		}
	} else if _, err := c.Head(ctx, repo, branch); err != nil {
		return "", "", "", "", fmt.Errorf("can't read branch %s of %q: %w", branch, repo, err)
	}
	return connID, repo, branch, tags, nil
}

// vcsConn opens a connection the workspace (by name) may link.
func (s *Server) vcsConn(ctx context.Context, connID, workspace string) (vcs.Conn, error) {
	sc, err := s.Store.GetVCSConnection(ctx, connID)
	if errors.Is(err, sql.ErrNoRows) {
		return vcs.Conn{}, errors.New("the VCS connection no longer exists")
	}
	if err != nil {
		return vcs.Conn{}, err
	}
	if !sc.AllowsWorkspace(workspace) {
		return vcs.Conn{}, fmt.Errorf("workspace %q may not link repositories through VCS connection %s: ask an admin", workspace, sc.Name)
	}
	return vcs.ConnFor(s.SecretKey, sc)
}

// newWorkspacePage is a wizard, like HCP Terraform's: ?type= picks the workflow (vcs, cli, template),
// then for vcs ?conn= the connection and ?repo= the repository, before the settings form.
func (s *Server) newWorkspacePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// "From a template" goes to that template's form.
	if id := q.Get("template"); id != "" {
		http.Redirect(w, r, "/templates/"+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	form := map[string]string{"type": q.Get("type"), "vcs_connection": q.Get("conn"), "vcs_repo": q.Get("repo"), "other": q.Get("other"),
		"vcs_branch": "main", "vcs_trigger": "1"}
	fillTagForm(form, "")
	if repo := form["vcs_repo"]; repo != "" {
		form["name"] = strings.ToLower(path.Base(repo)) // a suggestion; the form checks it
	}
	s.renderNewWorkspace(w, r, http.StatusOK, form, "")
}

func (s *Server) renderNewWorkspace(w http.ResponseWriter, r *http.Request, status int, form map[string]string, errMsg string) {
	reposOf := "" // only the repository step lists repositories, of its connection
	if form["type"] == "vcs" && form["vcs_repo"] == "" {
		reposOf = form["vcs_connection"]
	}
	f, err := s.loadVCSForm(r.Context(), form, errMsg, "", reposOf)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	data := map[string]any{"Templates": s.Templates.List(), "VCS": f, "Providers": vcs.Providers}
	for _, c := range f.Conns {
		if c.ID == form["vcs_connection"] {
			data["Conn"] = c
		}
	}
	s.render(w, r, status, "newworkspace.html", data)
}

// createEmptyWorkspace creates a workspace without a template, optionally linked to a branch.
// It queues no run: the next step is usually variables or a branch.
func (s *Server) createEmptyWorkspace(w http.ResponseWriter, r *http.Request) {
	form := vcsFormValues(r)
	if form["type"] == "" { // re-render a failed form at the right step
		form["type"] = map[bool]string{true: "vcs", false: "cli"}[form["vcs_connection"] != ""]
	}
	fail := func(msg string) { s.renderNewWorkspace(w, r, http.StatusUnprocessableEntity, form, msg) }
	name := form["name"]
	if !validWorkspaceName(name) {
		fail("Workspace name: " + workspaceNameRule)
		return
	}
	connID, repo, branch, tags, err := s.vcsLink(r.Context(), name, form)
	if err != nil {
		fail(err.Error())
		return
	}
	baseline, err := s.tagBaseline(r.Context(), connID, repo, tags)
	if err != nil {
		fail(err.Error())
		return
	}
	u := auth.FromContext(r.Context())
	ws, err := s.Store.CreateEmptyWorkspace(r.Context(), name, form["auto_apply"] != "", u.ID)
	if errors.Is(err, storage.ErrNameTaken) {
		fail(err.Error())
		return
	}
	if err == nil && connID != "" {
		err = s.Store.SetWorkspaceVCS(r.Context(), ws.ID, connID, repo, branch, tags, baseline, form["vcs_trigger"] != "" || tags != "", form["vcs_pull_requests"] != "" && tags == "")
	}
	if err != nil {
		s.internalError(w, "create workspace", err)
		return
	}
	s.Log.Info("workspace created", "workspace", ws.Name, "repo", repo, "branch", branch, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// loadLinkable loads a workspace whose repository link may change: operate access, no template,
// and no active run (its apply must fetch from the repository its plan used).
func (s *Server) loadLinkable(w http.ResponseWriter, r *http.Request) (storage.Workspace, bool) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return ws, false
	}
	if ws.TemplateID != "" {
		http.Error(w, "this workspace gets its configuration from its template", http.StatusConflict)
		return ws, false
	}
	return ws, true
}

// workspaceVCSPage is the workspace's version control settings, like HCP Terraform's: a linked
// workspace sees its repository and branch settings; "Change source" (/vcs/edit, or any unlinked
// workspace) is a wizard: ?type= the workflow, then for vcs ?conn= the connection and ?repo= the repository.
func (s *Server) workspaceVCSPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadLinkable(w, r)
	if !ok {
		return
	}
	branch := ws.VCSBranch
	if branch == "" {
		branch = "main"
	}
	form := map[string]string{"vcs_connection": ws.VCSConnectionID, "vcs_repo": ws.VCSRepo, "vcs_branch": branch}
	if ws.VCSTrigger || ws.VCSConnectionID == "" { // on by default for a new link, as in HCP Terraform
		form["vcs_trigger"] = "1"
	}
	if ws.VCSPullRequests {
		form["vcs_pull_requests"] = "1"
	}
	fillTagForm(form, ws.VCSTagsRegex)
	if q := r.URL.Query(); strings.HasSuffix(r.URL.Path, "/edit") || ws.VCSConnectionID == "" {
		form["type"], form["vcs_connection"], form["vcs_repo"] = q.Get("type"), q.Get("conn"), q.Get("repo")
		form["edit"] = "1"
	}
	s.renderWorkspaceVCS(w, r, http.StatusOK, ws, form, "")
}

func (s *Server) renderWorkspaceVCS(w http.ResponseWriter, r *http.Request, status int, ws storage.Workspace, form map[string]string, errMsg string) {
	edit := form["edit"] != "" || form["type"] != ""
	reposOf := "" // only the wizard's repository step lists repositories, of its connection
	if edit && form["type"] == "vcs" && form["vcs_repo"] == "" {
		reposOf = form["vcs_connection"]
	}
	f, err := s.loadVCSForm(r.Context(), form, errMsg, ws.Name, reposOf)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	data := map[string]any{"W": ws, "VCS": f, "Edit": edit, "Providers": vcs.Providers,
		"CanOperate": true, "Page": "vcs"} // CanOperate, Page: the settings menu
	if edit {
		for _, c := range f.Conns {
			if c.ID == form["vcs_connection"] {
				data["Conn"] = c
			}
		}
	} else {
		data["Conn"], data["Health"] = s.vcsHealth(r.Context(), ws)
	}
	s.render(w, r, status, "workspacevcs.html", data)
}

func (s *Server) linkWorkspaceVCS(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadLinkable(w, r)
	if !ok {
		return
	}
	form := vcsFormValues(r)
	connID, repo, branch, tags, err := s.vcsLink(r.Context(), ws.Name, form)
	var baseline string
	if err == nil && (connID != ws.VCSConnectionID || repo != ws.VCSRepo || tags != ws.VCSTagsRegex) {
		baseline, err = s.tagBaseline(r.Context(), connID, repo, tags)
	}
	if err != nil {
		s.renderWorkspaceVCS(w, r, http.StatusUnprocessableEntity, ws, form, err.Error())
		return
	}
	// tag-based: tags always trigger, and pull requests get no plans, as in HCP Terraform
	err = s.Store.SetWorkspaceVCS(r.Context(), ws.ID, connID, repo, branch, tags, baseline, form["vcs_trigger"] != "" || tags != "", form["vcs_pull_requests"] != "" && tags == "")
	if errors.Is(err, storage.ErrRunActive) {
		s.renderWorkspaceVCS(w, r, http.StatusConflict, ws, form, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "link workspace vcs", err)
		return
	}
	s.Log.Info("workspace vcs", "workspace", ws.Name, "repo", repo, "branch", branch,
		"trigger", form["vcs_trigger"] != "", "tags", tags, "pull_requests", form["vcs_pull_requests"] != "", "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// tagBaseline is the commit of the repository's newest tag matching tags ("" for branch-based):
// tag triggers start with the tags pushed after it, so switching to them doesn't apply an old release.
func (s *Server) tagBaseline(ctx context.Context, connID, repo, tags string) (string, error) {
	if tags == "" {
		return "", nil
	}
	sc, err := s.Store.GetVCSConnection(ctx, connID)
	if err != nil {
		return "", fmt.Errorf("load connection: %w", err)
	}
	c, err := vcs.ConnFor(s.SecretKey, sc)
	if err != nil {
		return "", err
	}
	list, err := c.Tags(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("list the repository's tags: %w", err)
	}
	t, _ := vcs.LatestTag(list, tags)
	return t.Commit, nil
}

// vcsHealth checks that a linked workspace can read its source as runs do, for the version control
// page's "Connected to VCS" mark: its connection and, if the workspace may use it, its branch or tags.
// It returns the connection, if it still exists, and the problem ("" if none).
func (s *Server) vcsHealth(ctx context.Context, ws storage.Workspace) (*storage.VCSConnection, string) {
	sc, err := s.Store.GetVCSConnection(ctx, ws.VCSConnectionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "its VCS connection was deleted"
	}
	if err != nil {
		return nil, err.Error()
	}
	if !sc.AllowsWorkspace(ws.Name) {
		return &sc, "an admin no longer allows this workspace to use " + sc.Name + ": its runs fail. Change source, or ask an admin to attach it under VCS providers."
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := vcs.ConnFor(s.SecretKey, sc)
	if err == nil && ws.VCSTagsRegex != "" {
		_, err = c.Tags(ctx, ws.VCSRepo)
	} else if err == nil {
		_, err = c.Head(ctx, ws.VCSRepo, ws.VCSBranch)
	}
	if err != nil {
		return &sc, err.Error()
	}
	return &sc, ""
}

// pinCommit records the commit a plan job runs, unless the run was queued with its commit: the head
// of the workspace's branch, or for tag-based workspaces the newest matching tag.
func (s *Server) pinCommit(ctx context.Context, j storage.Job) error {
	ws, err := s.Store.GetWorkspaceByID(ctx, j.WorkspaceID)
	if err != nil {
		return err
	}
	c, err := s.vcsConn(ctx, ws.VCSConnectionID, ws.Name) // fails if the admin deselected the workspace since the link
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
	_, err = s.Store.PinVCSCommit(ctx, j.RunID, head)
	return err
}

// vcsArchivePath ends in .tar.gz so Terraform's getter unpacks it; under /v1/, it's public and
// authorised by its signature.
func vcsArchivePath(runID string) string { return "/v1/vcs-archives/" + runID + ".tar.gz" }

// vcsArchive serves a run's pinned commit of its workspace's repository to the runner.
func (s *Server) vcsArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
	if !ok || !s.Registry.VerifyArchiveURL(r.URL.Path, q.Get("expires"), q.Get("sig")) {
		http.Error(w, "invalid or expired download link", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	run, _, err := s.Store.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.VCSCommit == "") {
		http.NotFound(w, r)
		return
	}
	var ws storage.Workspace
	if err == nil {
		ws, err = s.Store.GetWorkspaceByID(ctx, run.WorkspaceID)
	}
	if err != nil {
		s.internalError(w, "vcs archive", err)
		return
	}
	c, err := s.vcsConn(ctx, ws.VCSConnectionID, ws.Name)
	var b []byte
	if err == nil {
		b, err = c.Archive(ctx, ws.VCSRepo, run.VCSCommit, "commit")
	}
	if err != nil {
		s.Log.Warn("vcs archive", "run", run.ID, "repo", ws.VCSRepo, "err", err)
		http.Error(w, "fetching the repository failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Write(b)
}

// rollbackState makes a copy of an older state version the workspace's current state:
// same content, serial one above the current one, older versions kept.
// It holds the state lock and refuses during a run, so no run writes or applies a plan meanwhile.
func (s *Server) rollbackState(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	ctx := r.Context()
	from, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u := auth.FromContext(ctx)
	lockID := rand.Text()
	info, _ := json.Marshal(map[string]string{"ID": lockID, "Operation": "OperationTypeRollback", "Who": u.Email})
	if _, ok, err := s.Store.LockState(ctx, ws.ID, lockID, "", info); err != nil {
		s.internalError(w, "lock state", err)
		return
	} else if !ok {
		http.Error(w, "the state is locked", http.StatusConflict)
		return
	}
	defer func() {
		if err := s.Store.UnlockState(context.WithoutCancel(ctx), ws.ID, lockID); err != nil {
			s.Log.Error("unlock state", "workspace", ws.Name, "err", err)
		}
	}()
	// After locking: a run that starts now can't touch the state until the rollback is done, and one
	// that is between phases (e.g. waiting for confirmation) would apply a plan made against the old state.
	runs, err := s.Store.Runs(ctx, ws.ID, 1)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	if len(runs) > 0 && runs[0].Active() {
		http.Error(w, storage.ErrRunActive.Error(), http.StatusConflict)
		return
	}
	key, err := s.Store.StateVersionBlob(ctx, ws.ID, from)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "state version", err)
		return
	}
	cur, err := s.Store.StateVersions(ctx, ws.ID, 1)
	if err != nil {
		s.internalError(w, "list state versions", err)
		return
	}
	if cur[0].ID == from {
		http.Error(w, "this is already the current state version", http.StatusConflict)
		return
	}
	old, err := s.readBlob(ctx, key)
	if err != nil {
		s.internalError(w, "read state", err)
		return
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(old, &state); err != nil {
		s.internalError(w, "parse state", err)
		return
	}
	state["serial"] = json.RawMessage(strconv.FormatInt(cur[0].Serial+1, 10))
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		s.internalError(w, "encode state", err)
		return
	}
	if code, msg := s.saveState(ctx, ws.ID, "", body); code != http.StatusOK {
		http.Error(w, msg, code) // e.g. a version of another lineage
		return
	}
	if err := s.Store.CopyStateIndex(ctx, ws.ID, from); err != nil {
		s.Log.Error("copy state index", "workspace", ws.Name, "err", err) // the next apply indexes it
	}
	s.Log.Info("state rolled back", "workspace", ws.Name, "version", from, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}
