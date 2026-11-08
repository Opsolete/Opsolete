package httpserver

import (
	"net/http"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// Projects group workspaces. Admins create, rename and delete them and move workspaces (app decides);
// the audit middleware records each of those requests.
func (s *Server) projectRoutes(mux routes) {
	mux.HandleFunc("GET /projects", s.projects)
	mux.HandleFunc("POST /projects", s.createProject)
	// the project's menu pages: project.html shows the part of the tab
	mux.HandleFunc("GET /projects/{project}", s.projectTab("overview"))
	mux.HandleFunc("GET /projects/{project}/workspaces", s.projectTab("workspaces"))
	mux.HandleFunc("GET /projects/{project}/settings", s.projectTab("settings"))
	mux.HandleFunc("GET /projects/{project}/settings/{page}", s.projectTab("settings"))
	mux.HandleFunc("POST /projects/{project}/settings", s.projectSettings)
	mux.HandleFunc("POST /projects/{project}/delete", s.deleteProject)
	mux.HandleFunc("POST /projects/{project}/access", s.projectAccess)
	mux.HandleFunc("POST /workspaces/{name}/project", s.moveWorkspace)
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	s.projectsPage(w, r, http.StatusOK, nil)
}

// projectsPage lists the projects; f: a refused new project.
func (s *Server) projectsPage(w http.ResponseWriter, r *http.Request, status int, f *formState) {
	list, err := s.App.Projects(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, status, "projects.html", &projectsPage{Page{Section: "projects"}, list, s.Organization, f})
}

// projectsPage is the project list (projects.html).
type projectsPage struct {
	Page
	Projects []storage.Project
	Org      string
	F        *formState // a refused new project
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	p, err := s.App.CreateProject(r.Context(), actor(r), f.Get("name"), f.Get("description"))
	if s.failed(w, r, err, "project", f, func(status int, fs *formState) { s.projectsPage(w, r, status, fs) }) {
		return
	}
	http.Redirect(w, r, "/projects/"+p.Name, http.StatusSeeOther)
}

// projectPage is a project's menu page (project.html): Overview, Workspaces and Settings (General,
// Team access, Variable sets). Everyone who sees the project sees the first two; only admins the settings.
type projectPage struct {
	Page
	P          storage.Project
	F          *formState          // a refused form on the page
	Workspaces []storage.Workspace // overview (the most recently active ones), workspaces
	CanCreate  bool                // overview, workspaces: the user may create workspaces in it
	Grants     []storage.Grant     // settings: team access
	Groups     []string            // settings: team access, known groups to grant
	Sets       []storage.VariableSet
}

// projectTab serves the {project}'s menu page tab; "settings" also its sub-pages ({page}).
func (s *Server) projectTab(tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderProject(w, r, tab, r.PathValue("page"), http.StatusOK, nil)
	}
}

// onProjectPage re-renders a refused form on the {project}'s settings sub-page sub ("" General).
func (s *Server) onProjectPage(w http.ResponseWriter, r *http.Request, sub string) func(int, *formState) {
	return func(status int, f *formState) { s.renderProject(w, r, "settings", sub, status, f) }
}

// renderProject renders the {project}'s menu page tab (sub: the settings sub-page, "" General); f: a refused form on it.
func (s *Server) renderProject(w http.ResponseWriter, r *http.Request, tab, sub string, status int, f *formState) {
	if !slices.Contains([]string{"", "access", "variable-sets"}, sub) {
		http.NotFound(w, r)
		return
	}
	ctx, a := r.Context(), actor(r)
	load := s.App.Project
	if tab == "settings" {
		load = s.App.ProjectForSettings
	}
	p, err := load(ctx, a, r.PathValue("project"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := &projectPage{Page: Page{Section: "projects", InProject: r.PathValue("project"), Tab: tab, Sub: sub, WorkspaceCount: p.Workspaces}, P: p, F: f}
	switch {
	case tab == "overview" || tab == "workspaces":
		list, err := s.App.ListWorkspaces(ctx, a, p.Name)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		creatable, err := s.creatableProjects(r)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		data.CanCreate = slices.ContainsFunc(creatable, func(c storage.Project) bool { return c.ID == p.ID })
		if tab == "overview" { // the overview shows the most recently active ones
			list = list[:min(len(list), 5)]
		}
		data.Workspaces = list
	case sub == "access":
		acc, err := s.App.ProjectAccess(ctx, a, p.Name)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		data.Grants, data.Groups = acc.ProjectGrants, acc.Groups
	case sub == "variable-sets":
		if data.Sets, err = s.App.ProjectVariableSets(ctx, a, p.Name); err != nil {
			s.uiError(w, r, err)
			return
		}
	}
	s.render(w, r, status, "project.html", data)
}

// projectAccess grants an IdP group read or operate access to all of a project's workspaces
// (level "" revokes). Admins only.
func (s *Server) projectAccess(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	err := s.App.SetProjectGrant(r.Context(), actor(r), project, f.Get("group"), f.Get("level"))
	if s.failed(w, r, err, "access", f, s.onProjectPage(w, r, "access")) {
		return
	}
	http.Redirect(w, r, "/projects/"+project+"/settings/access", http.StatusSeeOther)
}

// projectSettings renames a project, sets its description and, with default=on, makes it the default.
func (s *Server) projectSettings(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	set := app.ProjectSettings{Name: f.Get("name"), Description: f.Get("description"), Default: f.Get("default") != ""}
	err := s.App.UpdateProject(r.Context(), actor(r), r.PathValue("project"), set)
	if s.failed(w, r, err, "general", f, s.onProjectPage(w, r, "")) {
		return
	}
	http.Redirect(w, r, "/projects/"+strings.TrimSpace(set.Name)+"/settings", http.StatusSeeOther)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteProject(r.Context(), actor(r), r.PathValue("project")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// moveWorkspace puts a workspace into another project, also during a run. Admins only.
func (s *Server) moveWorkspace(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.App.MoveWorkspace(r.Context(), actor(r), r.PathValue("name"), f.Get("project"))
	if s.failed(w, r, err, "project", f, s.onWorkspacePage(w, r, "settings", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings", http.StatusSeeOther)
}

// projectLabel names a project in the VCS and variable set pickers, the default one marked.
func projectLabel(p storage.Project) string {
	if p.Default {
		return p.Name + " (default)"
	}
	return p.Name
}

// canCreateWorkspaces: the user may create a workspace somewhere, for the "new workspace" entry points.
func (s *Server) canCreateWorkspaces(r *http.Request) (bool, error) {
	list, err := s.creatableProjects(r)
	return len(list) > 0, err
}

// creatableProjects lists the projects the user may create workspaces in, to pick one.
func (s *Server) creatableProjects(r *http.Request) ([]storage.Project, error) {
	if s.App.Store == nil { // only the DB-less tests
		return nil, nil
	}
	return s.App.CreatableProjects(r.Context(), actor(r))
}
