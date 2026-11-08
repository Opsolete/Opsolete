package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Workspaces' own configuration: none (empty, or holding only variables, or only added templates),
// or a VCS branch. Runs need configuration (an added template or a branch).

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// vcsForm is the repository part of the new-workspace and link forms.
type vcsForm struct {
	Conns []storage.VCSConnection
	Repos []string // of all connections, for the repo dropdown
	Form  map[string]string
	Error string
}

// loadVCSForm lists the connections the workspace (by name, "" = a new one) in project (by name) may
// link and the repos of reposOf (a connection id, "" = none); with VCS off it's empty and the form hides the part.
func (s *Server) loadVCSForm(ctx context.Context, form map[string]string, errMsg, workspace, project, reposOf string) (vcsForm, error) {
	f := vcsForm{Form: form, Error: errMsg}
	if !s.Features.VCS {
		return f, nil
	}
	conns, err := s.App.Store.VCSConnections(ctx)
	if err != nil {
		return f, err
	}
	for _, sc := range conns {
		if !sc.Allows(workspace, project) {
			continue
		}
		f.Conns = append(f.Conns, sc)
		if reposOf != sc.ID.String() {
			continue
		}
		c, err := vcs.ConnFor(s.App.SecretKey, sc)
		if err == nil {
			var repos []string
			if repos, err = c.Repos(ctx); err == nil {
				f.Repos = append(f.Repos, repos...)
			}
		}
		if err != nil { // typing the repo still works
			s.App.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
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

// tagFormats are the tag-based trigger formats offered, as the patterns they stand for.
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

// vcsLink validates the form's repository link for the workspace (by name, "" = a new one) in project
// (by name) and checks the branch exists, or for tag-based triggers (tags: their pattern) that the tags
// can be read; no connection = no link.
func (s *Server) vcsLink(ctx context.Context, workspace, project string, form map[string]string) (connID uuid.UUID, repo, branch, tags string, err error) {
	repo, branch = form["vcs_repo"], form["vcs_branch"]
	if form["vcs_connection"] == "" {
		return uuid.Nil, "", "", "", nil
	}
	if !s.Features.VCS {
		return uuid.Nil, "", "", "", errors.New("VCS connections are disabled")
	}
	connID, _ = parseID(form["vcs_connection"]) // not an id: VCSConn finds no such connection
	if branch == "" {
		branch = "main"
	}
	if !branchRe.MatchString(branch) || strings.Contains(branch, "..") {
		return uuid.Nil, "", "", "", fmt.Errorf("%q is not a branch name", branch)
	}
	if form["vcs_trigger_type"] == "tag" {
		if tags = tagFormats[form["vcs_tag_format"]]; tags == "" {
			tags = strings.TrimSpace(form["vcs_tags_regex"])
			if _, err := regexp.Compile(tags); err != nil || tags == "" || len(tags) > 255 {
				return uuid.Nil, "", "", "", fmt.Errorf("%q is not a tag pattern: use a regular expression such as ^v\\d+\\.\\d+\\.\\d+$", tags)
			}
		}
	}
	c, err := s.App.VCSConn(ctx, connID, workspace, project)
	if err != nil {
		return uuid.Nil, "", "", "", err
	}
	if tags != "" {
		if _, err := c.Tags(ctx, repo); err != nil {
			return uuid.Nil, "", "", "", fmt.Errorf("can't read the tags of %q: %w", repo, err)
		}
	} else if _, err := c.Head(ctx, repo, branch); err != nil {
		return uuid.Nil, "", "", "", fmt.Errorf("can't read branch %s of %q: %w", branch, repo, err)
	}
	return connID, repo, branch, tags, nil
}

// newWorkspacePage is a wizard: ?type= picks the workflow (vcs, cli, template),
// then for vcs ?conn= the connection and ?repo= the repository, before the settings form.
func (s *Server) newWorkspacePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// "From a template" goes to that template's form.
	if id := q.Get("template"); id != "" {
		to := "/templates/" + url.PathEscape(id)
		if p := q.Get("project"); p != "" {
			to += "?project=" + url.QueryEscape(p)
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	form := map[string]string{"type": q.Get("type"), "vcs_connection": q.Get("conn"), "vcs_repo": q.Get("repo"), "other": q.Get("other"),
		"vcs_branch": "main", "vcs_trigger": "1", "project": q.Get("project")}
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
	projects, err := s.creatableProjects(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	// ?project= is a URL parameter: one the user can't create in falls back to the default, else its connections' repos would list
	if !slices.ContainsFunc(projects, func(p storage.Project) bool { return p.Name == form["project"] }) {
		form["project"] = ""
	}
	project, err := s.App.NewWorkspaceProject(r.Context(), actor(r), form["project"], uuid.Nil)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	f, err := s.loadVCSForm(r.Context(), form, errMsg, "", project.Name, reposOf)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	data := map[string]any{"Templates": s.App.Templates.List(), "VCS": f, "Providers": vcs.Providers, "Projects": projects, "Project": form["project"]}
	for _, c := range f.Conns {
		if c.ID.String() == form["vcs_connection"] {
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
	form["project"] = r.PostFormValue(fieldProject)
	fail := func(msg string) { s.renderNewWorkspace(w, r, http.StatusUnprocessableEntity, form, msg) }
	// first: the VCS check needs the project; one the user can't pick is a bad request (the picker only offers the others)
	p, err := s.App.NewWorkspaceProject(r.Context(), actor(r), form["project"], uuid.Nil)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	// "": selected-scope connections list existing workspaces, so a new one only gets "all" connections
	connID, repo, branch, tags, err := s.vcsLink(r.Context(), "", p.Name, form)
	if err != nil {
		fail(err.Error())
		return
	}
	baseline, err := s.tagBaseline(r.Context(), connID, repo, tags)
	if err != nil {
		fail(err.Error())
		return
	}
	in := app.EmptyWorkspace{Name: form["name"], Project: p.Name, AutoApply: form["auto_apply"] != ""}
	if connID != uuid.Nil {
		in.VCS = &storage.VCSLink{ConnectionID: connID, Repo: repo, Branch: branch, TagsRegex: tags, TagBaseline: baseline,
			Trigger: form["vcs_trigger"] != "" || tags != "", PullRequests: form["vcs_pull_requests"] != "" && tags == ""}
	}
	ws, err := s.App.CreateEmptyWorkspace(r.Context(), actor(r), in)
	if isValidation(err) {
		fail(err.Error())
		return
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// workspaceVCSPage is the workspace's version control settings: a linked
// workspace sees its repository and branch settings; "Change source" (/vcs/edit, or any unlinked
// workspace) is a wizard: ?type= the workflow, then for vcs ?conn= the connection and ?repo= the repository.
func (s *Server) workspaceVCSPage(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, app.LevelOperate)
	if !ok {
		return
	}
	branch := ws.VCSBranch
	if branch == "" {
		branch = "main"
	}
	form := map[string]string{"vcs_connection": formID(ws.VCSConnectionID), "vcs_repo": ws.VCSRepo, "vcs_branch": branch}
	if ws.VCSTrigger || ws.VCSConnectionID == uuid.Nil { // on by default for a new link
		form["vcs_trigger"] = "1"
	}
	if ws.VCSPullRequests {
		form["vcs_pull_requests"] = "1"
	}
	fillTagForm(form, ws.VCSTagsRegex)
	if q := r.URL.Query(); strings.HasSuffix(r.URL.Path, "/edit") || ws.VCSConnectionID == uuid.Nil {
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
	f, err := s.loadVCSForm(r.Context(), form, errMsg, ws.Name, ws.ProjectName, reposOf)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	data := map[string]any{"W": ws, "VCS": f, "Edit": edit, "Providers": vcs.Providers,
		"CanOperate": true, "Page": "vcs"} // CanOperate, Page: the settings menu
	if edit {
		for _, c := range f.Conns {
			if c.ID.String() == form["vcs_connection"] {
				data["Conn"] = c
			}
		}
	} else {
		data["Conn"], data["Health"] = s.vcsHealth(r.Context(), ws)
	}
	s.render(w, r, status, "workspacevcs.html", data)
}

func (s *Server) linkWorkspaceVCS(w http.ResponseWriter, r *http.Request) {
	// SetWorkspaceVCS refuses a change during an active run: its apply must fetch from the repository its plan used.
	ws, _, ok := s.loadWorkspace(w, r, app.LevelOperate)
	if !ok {
		return
	}
	form := vcsFormValues(r)
	connID, repo, branch, tags, err := s.vcsLink(r.Context(), ws.Name, ws.ProjectName, form)
	var baseline string
	if err == nil && (connID != ws.VCSConnectionID || repo != ws.VCSRepo || tags != ws.VCSTagsRegex) {
		baseline, err = s.tagBaseline(r.Context(), connID, repo, tags)
	}
	if err != nil {
		s.renderWorkspaceVCS(w, r, http.StatusUnprocessableEntity, ws, form, err.Error())
		return
	}
	// tag-based: tags always trigger, and pull requests get no plans
	err = s.App.Store.SetWorkspaceVCS(r.Context(), ws.ID, connID, repo, branch, tags, baseline, form["vcs_trigger"] != "" || tags != "", form["vcs_pull_requests"] != "" && tags == "")
	if errors.Is(err, storage.ErrRunActive) {
		s.renderWorkspaceVCS(w, r, http.StatusConflict, ws, form, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "link workspace vcs", err)
		return
	}
	s.App.Log.Info("workspace vcs", "workspace", ws.Name, "repo", repo, "branch", branch,
		"trigger", form["vcs_trigger"] != "", "tags", tags, "pull_requests", form["vcs_pull_requests"] != "", "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// tagBaseline is the commit of the repository's newest tag matching tags ("" for branch-based):
// tag triggers start with the tags pushed after it, so switching to them doesn't apply an old release.
func (s *Server) tagBaseline(ctx context.Context, connID uuid.UUID, repo, tags string) (string, error) {
	if tags == "" {
		return "", nil
	}
	sc, err := s.App.Store.GetVCSConnection(ctx, connID)
	if err != nil {
		return "", fmt.Errorf("load connection: %w", err)
	}
	c, err := vcs.ConnFor(s.App.SecretKey, sc)
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
	sc, err := s.App.Store.GetVCSConnection(ctx, ws.VCSConnectionID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, "its VCS connection was deleted"
	}
	if err != nil {
		return nil, err.Error()
	}
	if !sc.Allows(ws.Name, ws.ProjectName) {
		return &sc, "an admin no longer allows this workspace to use " + sc.Name + ": its runs fail. Change source, or ask an admin to attach it under VCS providers."
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := vcs.ConnFor(s.App.SecretKey, sc)
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

// vcsArchive serves a run's pinned commit of its workspace's repository to the runner.
func (s *Server) vcsArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
	if !ok || !s.App.Registry.VerifyArchiveURL(r.URL.Path, q.Get("expires"), q.Get("sig")) {
		http.Error(w, "invalid or expired download link", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	id, _ := parseID(runID) // not an id: not found
	run, _, err := s.App.Store.GetRun(ctx, id)
	if errors.Is(err, storage.ErrNotFound) || (err == nil && run.VCSCommit == "") {
		http.NotFound(w, r)
		return
	}
	var ws storage.Workspace
	if err == nil {
		ws, err = s.App.Store.GetWorkspaceByID(ctx, run.WorkspaceID)
	}
	if err != nil {
		s.internalError(w, "vcs archive", err)
		return
	}
	c, err := s.App.VCSConn(ctx, ws.VCSConnectionID, ws.Name, ws.ProjectName)
	var b []byte
	if err == nil {
		b, err = c.Archive(ctx, ws.VCSRepo, run.VCSCommit, "commit", "")
	}
	if err != nil {
		s.App.Log.Warn("vcs archive", "run", run.ID, "repo", ws.VCSRepo, "err", err)
		http.Error(w, "fetching the repository failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Write(b)
}

// rollbackState makes a copy of an older state version the workspace's current state (app.State.Rollback).
func (s *Server) rollbackState(w http.ResponseWriter, r *http.Request) {
	from, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, err := s.App.State(r.Context(), actor(r), r.PathValue("name"), app.LevelOperate)
	if err == nil {
		err = st.Rollback(r.Context(), from)
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+st.Workspace.Name, http.StatusSeeOther)
}
