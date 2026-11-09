package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Workspaces' own configuration: none (empty, or holding only variables, or only added templates),
// or a VCS branch. Runs need configuration (an added template or a branch).

// vcsForm is the repository part of the new-workspace and link forms.
type vcsForm struct {
	Conns []storage.VCSConnection
	Repos []string // of all connections, for the repo dropdown
	Form  map[string]string
	F     *formState // a refused submit
}

// loadVCSForm lists the connections the workspace (by name, "" = a new one) in project (by name) may
// link and the repos of reposOf (a connection id, "" = none); with VCS off it's empty and the form hides the part.
func (s *Server) loadVCSForm(r *http.Request, form map[string]string, fs *formState, workspace, project, reposOf string) (vcsForm, error) {
	f := vcsForm{Form: form, F: fs}
	id, _ := parseID(reposOf)
	var err error
	f.Conns, f.Repos, err = s.App.LinkableVCS(r.Context(), actor(r), workspace, project, id)
	return f, err
}

// vcsFormValues reads the new-workspace and link forms; "name" is the workspace name (fieldWorkspace).
func vcsFormValues(f url.Values) map[string]string {
	form := trimmed(f, "type", "auto_apply", "vcs_connection", "vcs_repo", "vcs_branch", "vcs_trigger", "vcs_pull_requests",
		"vcs_trigger_type", "vcs_tag_format", "vcs_tags_regex")
	form["name"], form["project"] = strings.TrimSpace(f.Get(fieldWorkspace)), f.Get(fieldProject)
	return form
}

// fillTagForm sets the form's trigger type and tag format from a workspace's tag pattern.
func fillTagForm(form map[string]string, tagsRegex string) {
	form["vcs_trigger_type"], form["vcs_tag_format"], form["vcs_tags_regex"] = "branch", "semantic", ""
	if tagsRegex == "" {
		return
	}
	form["vcs_trigger_type"], form["vcs_tag_format"], form["vcs_tags_regex"] = "tag", "custom", tagsRegex
	for k, re := range app.TagFormats {
		if re == tagsRegex {
			form["vcs_tag_format"], form["vcs_tags_regex"] = k, ""
		}
	}
}

// vcsLinkInput reads the form's repository link; false: the connection field isn't an id.
func vcsLinkInput(form map[string]string) (app.VCSLinkInput, bool) {
	id, ok := parseID(form["vcs_connection"])
	return app.VCSLinkInput{ConnectionID: id, Repo: form["vcs_repo"], Branch: form["vcs_branch"], Tags: form["vcs_trigger_type"] == "tag",
		TagFormat: form["vcs_tag_format"], TagsRegex: form["vcs_tags_regex"], Trigger: form["vcs_trigger"] != "",
		PullRequests: form["vcs_pull_requests"] != ""}, ok || form["vcs_connection"] == ""
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
	s.renderNewWorkspace(w, r, http.StatusOK, form, nil)
}

// newWorkspacePage is the new-workspace wizard (newworkspace.html).
type newWorkspacePage struct {
	Page
	Templates []templates.Template // "from a template"
	VCS       vcsForm
	Providers any                    // vcs.Providers, to add a connection
	Conn      *storage.VCSConnection // the one picked, nil none
	Projects  []storage.Project      // to pick from; none: the user can't create workspaces
	Project   string                 // the picked one, a name; "" the default
	F         *formState             // a refused submit
}

// pickedConn is the connection of conns with id, nil if none.
func pickedConn(conns []storage.VCSConnection, id string) *storage.VCSConnection {
	for i := range conns {
		if conns[i].ID.String() == id {
			return &conns[i]
		}
	}
	return nil
}

func (s *Server) renderNewWorkspace(w http.ResponseWriter, r *http.Request, status int, form map[string]string, fs *formState) {
	reposOf := "" // only the repository step lists repositories, of its connection
	if form["type"] == "vcs" && form["vcs_repo"] == "" {
		reposOf = form["vcs_connection"]
	}
	projects, err := s.creatableProjects(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	if len(projects) == 0 { // the page says whom to ask
		s.render(w, r, status, "newworkspace.html", &newWorkspacePage{Page: Page{Section: "workspaces"}, VCS: vcsForm{Form: form, F: fs}, F: fs})
		return
	}
	// ?project= is a URL parameter: one the user can't create in falls back to the one the picker
	// preselects (the default if the user may create there, else the first), else its connections' repos would list
	if !slices.ContainsFunc(projects, func(p storage.Project) bool { return p.Name == form["project"] }) {
		form["project"] = ""
	}
	project := form["project"]
	if project == "" {
		project = projects[max(0, slices.IndexFunc(projects, func(p storage.Project) bool { return p.Default }))].Name
	}
	f, err := s.loadVCSForm(r, form, fs, "", project, reposOf)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := &newWorkspacePage{Page: Page{Section: "workspaces"}, Templates: s.App.Templates.List(), VCS: f, Providers: vcs.Providers, Projects: projects, Project: form["project"], F: fs,
		Conn: pickedConn(f.Conns, form["vcs_connection"])}
	s.render(w, r, status, "newworkspace.html", data)
}

// createEmptyWorkspace creates a workspace without a template, optionally linked to a branch.
// It queues no run: the next step is usually variables or a branch.
func (s *Server) createEmptyWorkspace(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	form := vcsFormValues(f)
	if form["type"] == "" { // re-render a failed form at the right step
		form["type"] = map[bool]string{true: "vcs", false: "cli"}[form["vcs_connection"] != ""]
	}
	link, ok := vcsLinkInput(form)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ws, err := s.App.CreateEmptyWorkspace(r.Context(), actor(r), app.EmptyWorkspace{Name: form["name"], Project: form["project"],
		AutoApply: form["auto_apply"] != "", VCS: link})
	if s.failed(w, r, err, "workspace", f, func(status int, fs *formState) { s.renderNewWorkspace(w, r, status, form, fs) }) {
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
	s.renderWorkspaceVCS(w, r, http.StatusOK, ws, form, nil)
}

// workspaceVCSPage is a workspace's version control settings (workspacevcs.html).
type workspaceVCSPage struct {
	Page
	W         storage.Workspace
	VCS       vcsForm
	Providers any                    // vcs.Providers
	Edit      bool                   // the wizard, not the linked repository's settings
	Conn      *storage.VCSConnection // the linked or picked one, nil none
	Health    string                 // not editing: what is wrong with the link, "" nothing
}

func (s *Server) renderWorkspaceVCS(w http.ResponseWriter, r *http.Request, status int, ws storage.Workspace, form map[string]string, fs *formState) {
	edit := form["edit"] != "" || form["type"] != ""
	reposOf := "" // only the wizard's repository step lists repositories, of its connection
	if edit && form["type"] == "vcs" && form["vcs_repo"] == "" {
		reposOf = form["vcs_connection"]
	}
	f, err := s.loadVCSForm(r, form, fs, ws.Name, ws.ProjectName, reposOf)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	// loadWorkspace and LinkVCS need operate: the menu offers the settings
	data := &workspaceVCSPage{Page: workspaceNav(r.PathValue("name"), "vcs", "vcs", true), W: ws, VCS: f, Providers: vcs.Providers, Edit: edit}
	if edit {
		data.Conn = pickedConn(f.Conns, form["vcs_connection"])
	} else if data.Conn, data.Health, err = s.App.VCSHealth(r.Context(), actor(r), ws.Name); err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, status, "workspacevcs.html", data)
}

// linkWorkspaceVCS links, relinks or unlinks the workspace's repository (app.LinkVCS); a refused link
// (or one during an active run, 409) re-renders the form.
func (s *Server) linkWorkspaceVCS(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	form := vcsFormValues(f)
	in, ok := vcsLinkInput(form)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ws, err := s.App.LinkVCS(r.Context(), actor(r), r.PathValue("name"), in)
	if s.failed(w, r, err, "vcs", f, func(status int, fs *formState) { s.renderWorkspaceVCS(w, r, status, ws, form, fs) }) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// vcsArchive serves a run's pinned commit to the runner (app.VCSArchive).
func (s *Server) vcsArchive(w http.ResponseWriter, r *http.Request) {
	file, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
	id, idOK := parseID(file)
	if !ok || !idOK {
		http.Error(w, "invalid or expired download link", http.StatusForbidden)
		return
	}
	b, err := s.App.VCSArchive(r.Context(), id, r.URL.Query().Get("expires"), r.URL.Query().Get("sig"))
	if errors.Is(err, app.ErrVCSFetch) {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err != nil {
		s.uiError(w, r, err)
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
