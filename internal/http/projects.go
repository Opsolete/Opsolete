package httpserver

import (
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
	mux.HandleFunc("GET /projects/{project}", s.project)
	mux.HandleFunc("POST /projects/{project}/settings", s.admin(s.projectSettings))
	mux.HandleFunc("POST /projects/{project}/delete", s.admin(s.deleteProject))
	mux.HandleFunc("POST /workspaces/{name}/project", s.admin(s.moveWorkspace))
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Projects(r.Context(), viewer(r))
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "projects.html", list)
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

func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadProject(w, r)
	if !ok {
		return
	}
	list, err := s.Store.ListWorkspaces(r.Context(), viewer(r), p.Name)
	if err != nil {
		s.internalError(w, "list workspaces", err)
		return
	}
	s.render(w, r, http.StatusOK, "project.html", map[string]any{"P": p, "Workspaces": list})
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
	http.Redirect(w, r, "/projects/"+name, http.StatusSeeOther)
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

// visibleProjects lists the projects the user may see, to pick one for a new workspace.
func (s *Server) visibleProjects(r *http.Request) ([]storage.Project, error) {
	if s.Store == nil { // only the DB-less tests
		return nil, nil
	}
	return s.Store.Projects(r.Context(), viewer(r))
}

// pickableProject checks a project name posted with a new workspace: "" (the default project) or a
// project the user may see. It writes a 400 or 500 and returns false otherwise.
func (s *Server) pickableProject(w http.ResponseWriter, r *http.Request, name string) bool {
	if name == "" {
		return true
	}
	list, err := s.visibleProjects(r)
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
