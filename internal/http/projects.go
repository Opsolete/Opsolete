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
	// the project's menu pages: one handler, project.html shows the part of the current tab
	mux.HandleFunc("GET /projects/{project}", s.project)
	mux.HandleFunc("GET /projects/{project}/workspaces", s.project)
	mux.HandleFunc("GET /projects/{project}/settings", s.project)
	mux.HandleFunc("GET /projects/{project}/settings/{page}", s.project)
	mux.HandleFunc("POST /projects/{project}/settings", s.projectSettings)
	mux.HandleFunc("POST /projects/{project}/delete", s.deleteProject)
	mux.HandleFunc("POST /projects/{project}/access", s.projectAccess)
	mux.HandleFunc("POST /workspaces/{name}/project", s.moveWorkspace)
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Projects(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "projects.html", map[string]any{"Projects": list, "Org": s.Organization})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	p, err := s.App.CreateProject(r.Context(), actor(r), r.PostForm.Get("name"), r.PostForm.Get("description"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Name, http.StatusSeeOther)
}

// project serves a project's menu: Overview, Workspaces and Settings (General,
// Team access, Variable sets). Everyone who sees the project sees them; only admins change them.
func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page") // settings sub-pages; "" is General
	if !slices.Contains([]string{"", "access", "variable-sets"}, page) {
		http.NotFound(w, r)
		return
	}
	ctx, a := r.Context(), actor(r)
	p, err := s.App.Project(ctx, a, r.PathValue("project"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := map[string]any{"P": p, "Page": page}
	tab, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/projects/"+p.Name), "/"), "/")
	switch {
	case tab == "" || tab == "workspaces":
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
		data["CanCreate"] = slices.ContainsFunc(creatable, func(c storage.Project) bool { return c.ID == p.ID })
		if tab == "" { // the overview shows the most recently active ones
			list = list[:min(len(list), 5)]
		}
		data["Workspaces"] = list
	case page == "access":
		acc, err := s.App.ProjectAccess(ctx, a, p.Name)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		data["Grants"], data["Groups"] = acc.ProjectGrants, acc.Groups
	case page == "variable-sets":
		if data["Sets"], err = s.App.ProjectVariableSets(ctx, a, p.Name); err != nil {
			s.uiError(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "project.html", data)
}

// projectAccess grants an IdP group read or operate access to all of a project's workspaces
// (level "" revokes). Admins only.
func (s *Server) projectAccess(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	project := r.PathValue("project")
	if err := s.App.SetProjectGrant(r.Context(), actor(r), project, r.PostForm.Get("group"), r.PostForm.Get("level")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects/"+project+"/settings/access", http.StatusSeeOther)
}

// projectSettings renames a project, sets its description and, with default=on, makes it the default.
func (s *Server) projectSettings(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	set := app.ProjectSettings{Name: r.PostForm.Get("name"), Description: r.PostForm.Get("description"), Default: r.PostForm.Get("default") != ""}
	if err := s.App.UpdateProject(r.Context(), actor(r), r.PathValue("project"), set); err != nil {
		s.uiError(w, r, err)
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
	if !parseForm(w, r) {
		return
	}
	if err := s.App.MoveWorkspace(r.Context(), actor(r), r.PathValue("name"), r.PostForm.Get("project")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings", http.StatusSeeOther)
}

// projectLabel names a project in the VCS and variable set pickers; the default one says it's open to every user.
func projectLabel(p storage.Project) string {
	if p.Default {
		return p.Name + " (default: every user may create workspaces here)"
	}
	return p.Name
}

// creatableProjects lists the projects the user may create workspaces in, to pick one.
func (s *Server) creatableProjects(r *http.Request) ([]storage.Project, error) {
	if s.App.Store == nil { // only the DB-less tests
		return nil, nil
	}
	return s.App.CreatableProjects(r.Context(), actor(r))
}
