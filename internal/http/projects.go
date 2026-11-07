package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
)

// Projects group workspaces. Admins create, rename and delete them and move workspaces; the
// audit middleware records each of those requests.
func (s *Server) projectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /projects", s.projects)
	mux.HandleFunc("POST /projects", s.admin(s.createProject))
	// the project's menu pages: one handler, project.html shows the part of the current tab
	mux.HandleFunc("GET /projects/{project}", s.project)
	mux.HandleFunc("GET /projects/{project}/workspaces", s.project)
	mux.HandleFunc("GET /projects/{project}/settings", s.project)
	mux.HandleFunc("GET /projects/{project}/settings/{page}", s.project)
	mux.HandleFunc("POST /projects/{project}/settings", s.admin(s.projectSettings))
	mux.HandleFunc("POST /projects/{project}/delete", s.admin(s.deleteProject))
	mux.HandleFunc("POST /projects/{project}/access", s.admin(s.projectAccess))
	mux.HandleFunc("POST /workspaces/{name}/project", s.admin(s.moveWorkspace))
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Projects(r.Context(), viewer(r))
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "projects.html", map[string]any{"Projects": list, "Org": s.Organization})
}

// projectFromForm reads and checks a project's name and description.
func projectFromForm(w http.ResponseWriter, r *http.Request) (name, description string, ok bool) {
	name, description = strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("description"))
	if !validWorkspaceName(name) { // the same rule: names are URL keys like workspace names
		http.Error(w, "a project name is "+workspaceNameRule, http.StatusBadRequest)
		return "", "", false
	}
	if utf8.RuneCountInString(description) > 1000 {
		http.Error(w, "the description is at most 1000 characters", http.StatusBadRequest)
		return "", "", false
	}
	return name, description, true
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	name, description, ok := projectFromForm(w, r)
	if !ok {
		return
	}
	p, err := s.Store.CreateProject(r.Context(), name, description)
	if errors.Is(err, storage.ErrProjectNameTaken) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, "create project", err)
		return
	}
	s.Log.Info("project created", "project", p.Name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/projects/"+p.Name, http.StatusSeeOther)
}

// loadProject loads the {project} project if the user may see it; a 404 otherwise.
func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (storage.Project, bool) {
	list, err := s.Store.Projects(r.Context(), viewer(r))
	if err != nil {
		s.internalError(w, "list projects", err)
		return storage.Project{}, false
	}
	i := slices.IndexFunc(list, func(p storage.Project) bool { return p.Name == r.PathValue("project") })
	if i < 0 {
		http.NotFound(w, r)
		return storage.Project{}, false
	}
	return list[i], true
}

// project serves a project's menu, like HCP Terraform's: Overview, Workspaces and Settings (General,
// Team access, Variable sets). Everyone who sees the project sees them; only admins change them.
func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page") // settings sub-pages; "" is General
	if !slices.Contains([]string{"", "access", "variable-sets"}, page) {
		http.NotFound(w, r)
		return
	}
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	data := map[string]any{"P": p, "Page": page}
	tab, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/projects/"+p.Name), "/"), "/")
	var err error
	switch {
	case tab == "" || tab == "workspaces":
		var list []storage.Workspace
		if list, err = s.Store.ListWorkspaces(ctx, viewer(r), p.Name); err != nil {
			s.internalError(w, "list workspaces", err)
			return
		}
		creatable, err := s.creatableProjects(r)
		if err != nil {
			s.internalError(w, "list projects", err)
			return
		}
		data["CanCreate"] = slices.ContainsFunc(creatable, func(c storage.Project) bool { return c.ID == p.ID })
		if tab == "" { // the overview shows the most recently active ones
			list = list[:min(len(list), 5)]
		}
		data["Workspaces"] = list
	case page == "access":
		if data["Grants"], err = s.Store.ProjectGrants(ctx, p.ID); err != nil {
			s.internalError(w, "project grants", err)
			return
		}
		if auth.FromContext(ctx).Admin() {
			if data["Groups"], err = s.Store.KnownGroups(ctx); err != nil {
				s.internalError(w, "known groups", err)
				return
			}
		}
	case page == "variable-sets":
		if data["Sets"], err = s.Store.ProjectVariableSets(ctx, p.ID); err != nil {
			s.internalError(w, "project variable sets", err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "project.html", data)
}

// projectAccess grants an IdP group read or operate access to all of a project's workspaces
// (level "" revokes). Admins only (enforced by the route).
func (s *Server) projectAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	group, level, ok := grantFromForm(w, r)
	if !ok {
		return
	}
	if err := s.Store.SetProjectGrant(r.Context(), p.ID, group, level); err != nil {
		s.internalError(w, "set project grant", err)
		return
	}
	s.Log.Info("project access", "project", p.Name, "group", group, "level", level, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/projects/"+p.Name+"/settings/access", http.StatusSeeOther)
}

// projectSettings renames a project, sets its description and, with default=on, makes it the default.
func (s *Server) projectSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	name, description, ok := projectFromForm(w, r)
	if !ok {
		return
	}
	if err := s.Store.UpdateProject(r.Context(), p.ID, name, description); errors.Is(err, storage.ErrProjectNameTaken) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	} else if err != nil {
		s.internalError(w, "update project", err)
		return
	}
	if r.PostFormValue("default") != "" && !p.Default {
		if err := s.Store.SetDefaultProject(r.Context(), p.ID); err != nil {
			s.internalError(w, "default project", err)
			return
		}
	}
	s.Log.Info("project updated", "project", p.Name, "name", name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/projects/"+name+"/settings", http.StatusSeeOther)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteProject(r.Context(), p.ID); errors.Is(err, storage.ErrProjectNotEmpty) || errors.Is(err, storage.ErrDefaultProject) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) { // gone meanwhile: deleted all the same
		s.internalError(w, "delete project", err)
		return
	}
	s.Log.Info("project deleted", "project", p.Name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

// moveWorkspace puts a workspace into another project, also during a run (see storage.MoveWorkspace).
func (s *Server) moveWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	p, err := s.Store.Project(r.Context(), r.PostFormValue("project"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no such project", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.internalError(w, "get project", err)
		return
	}
	if err := s.Store.MoveWorkspace(r.Context(), ws.ID, p.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.internalError(w, "move workspace", err)
		return
	}
	s.Log.Info("workspace moved", "workspace", ws.Name, "from", ws.ProjectName, "to", p.Name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/settings", http.StatusSeeOther)
}

// newWorkspaceProject is the name of the project a new workspace goes into: picked, else the default.
func (s *Server) newWorkspaceProject(ctx context.Context, picked string) (string, error) {
	if picked != "" {
		return picked, nil
	}
	p, err := s.Store.DefaultProject(ctx)
	return p.Name, err
}

// creatableProjects lists the projects the user may create workspaces in, to pick one.
func (s *Server) creatableProjects(r *http.Request) ([]storage.Project, error) {
	if s.Store == nil { // only the DB-less tests
		return nil, nil
	}
	return s.Store.CreatableProjects(r.Context(), viewer(r))
}

// pickableProject checks a project name posted with a new workspace: "" (the default project) or a
// project the user may create workspaces in. It writes a 400 or 500 and returns false otherwise.
func (s *Server) pickableProject(w http.ResponseWriter, r *http.Request, name string) bool {
	if name == "" {
		return true
	}
	list, err := s.creatableProjects(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return false
	}
	if !slices.ContainsFunc(list, func(p storage.Project) bool { return p.Name == name }) {
		http.Error(w, "no such project", http.StatusBadRequest)
		return false
	}
	return true
}
