package httpserver

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// Variables: the workspace's own (operate access) and variable sets (admins); the rules are app's.
func (s *Server) variableRoutes(mux routes) {
	mux.HandleFunc("POST /workspaces/{name}/variables", s.setWorkspaceVariable)
	mux.HandleFunc("POST /workspaces/{name}/variables/{id}/delete", s.deleteWorkspaceVariable)
	mux.HandleFunc("GET /variable-sets", s.variableSets)
	mux.HandleFunc("POST /variable-sets", s.createVariableSet)
	mux.HandleFunc("GET /variable-sets/{set}", s.variableSet)
	mux.HandleFunc("POST /variable-sets/{set}", s.updateVariableSet)
	mux.HandleFunc("POST /variable-sets/{set}/delete", s.deleteVariableSet)
	mux.HandleFunc("POST /variable-sets/{set}/variables", s.setSetVariable)
	mux.HandleFunc("POST /variable-sets/{set}/variables/{id}/delete", s.deleteSetVariable)
	mux.HandleFunc("POST /variable-sets/{set}/workspaces", s.attachVariableSet)
	mux.HandleFunc("POST /variable-sets/{set}/projects", s.attachVariableSetProjects)
}

// variableInput reads a posted variable.
func variableInput(f url.Values) app.VariableInput {
	return app.VariableInput{Category: f.Get("category"), Key: f.Get("key"), Value: f.Get("value"), Sensitive: f.Get("sensitive") != ""}
}

// varsBlock is the data of the "variables" partial.
type varsBlock struct {
	Base    string // URL the add and delete forms post under
	Vars    []app.Variable
	CanEdit bool
	F       *formState // a refused new variable
}

// setView is a variable set applied to a workspace, with its variables.
type setView struct {
	storage.VariableSet
	Vars varsBlock
}

func (s *Server) setWorkspaceVariable(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.App.SetWorkspaceVariable(r.Context(), actor(r), r.PathValue("name"), variableInput(f))
	if s.failed(w, r, err, "variable", f, s.onWorkspacePage(w, r, "/variables", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/variables", http.StatusSeeOther)
}

func (s *Server) deleteWorkspaceVariable(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: deletes nothing, as for an unknown one
	if err := s.App.DeleteWorkspaceVariable(r.Context(), actor(r), r.PathValue("name"), id); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/variables", http.StatusSeeOther)
}

func (s *Server) variableSets(w http.ResponseWriter, r *http.Request) {
	s.variableSetsPage(w, r, http.StatusOK, nil)
}

// variableSetsPage lists the sets; f: a refused new set.
func (s *Server) variableSetsPage(w http.ResponseWriter, r *http.Request, status int, f *formState) {
	sets, err := s.App.VariableSets(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, status, "variablesets.html", map[string]any{"Sets": sets, "F": f})
}

// createVariableSet creates a set; from a project's or workspace's page (project, workspace) it's
// attached there too. A refused one is shown on the sets page, its project or workspace kept.
func (s *Server) createVariableSet(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	in := app.NewVariableSet{Name: f.Get("name"), Description: f.Get("description"), Global: f.Get("global") != "",
		Project: f.Get("project"), Workspace: f.Get("workspace")}
	err := s.App.CreateVariableSet(r.Context(), actor(r), in)
	if s.failed(w, r, err, "set", f, func(status int, fs *formState) { s.variableSetsPage(w, r, status, fs) }) {
		return
	}
	http.Redirect(w, r, "/variable-sets/"+strings.TrimSpace(in.Name), http.StatusSeeOther)
}

func (s *Server) variableSet(w http.ResponseWriter, r *http.Request) {
	s.variableSetPage(w, r, http.StatusOK, nil)
}

// onSetPage re-renders a refused form on the {set}'s page.
func (s *Server) onSetPage(w http.ResponseWriter, r *http.Request) func(int, *formState) {
	return func(status int, f *formState) { s.variableSetPage(w, r, status, f) }
}

// variableSetPage renders the set's page; f: a refused form on it (its picks are shown as posted).
func (s *Server) variableSetPage(w http.ResponseWriter, r *http.Request, status int, f *formState) {
	ctx, a := r.Context(), actor(r)
	set, err := s.App.VariableSet(ctx, a, r.PathValue("set"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	// shortcut: offers the 200 most recently active workspaces; add search if there are more.
	all, err := s.App.ListWorkspaces(ctx, a, "") // an admin's: all of them
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	selected, picked := set.Workspaces, set.Projects
	if f := formOf(f, "workspaces"); f != nil {
		selected = f.Input["workspace"]
	}
	if f := formOf(f, "projects"); f != nil {
		picked = f.Input["project"]
	}
	p := workspacePicker("workspace", "Workspaces to apply the set to", selected, all)
	projects, err := s.App.Projects(ctx, a)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	var items []pickerItem
	for _, pr := range projects {
		items = append(items, pickerItem{pr.Name, projectLabel(pr), slices.Contains(picked, pr.Name)})
	}
	vars := varsBlock{Base: "/variable-sets/" + set.Name, Vars: set.Vars, CanEdit: true, F: formOf(f, "variable")}
	s.render(w, r, status, "variableset.html", map[string]any{"Set": set.VariableSet, "Variables": vars, "Picker": p, "Projects": items, "F": f})
}

func (s *Server) updateVariableSet(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	set := r.PathValue("set")
	err := s.App.UpdateVariableSet(r.Context(), actor(r), set, f.Get("description"), f.Get("global") != "")
	if s.failed(w, r, err, "set", f, s.onSetPage(w, r)) {
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}

func (s *Server) deleteVariableSet(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteVariableSet(r.Context(), actor(r), r.PathValue("set")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/variable-sets", http.StatusSeeOther)
}

func (s *Server) setSetVariable(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	set := r.PathValue("set")
	err := s.App.SetSetVariable(r.Context(), actor(r), set, variableInput(f))
	if s.failed(w, r, err, "variable", f, s.onSetPage(w, r)) {
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}

func (s *Server) deleteSetVariable(w http.ResponseWriter, r *http.Request) {
	set := r.PathValue("set")
	id, _ := pathID(r, "id") // not an id: deletes nothing, as for an unknown one
	if err := s.App.DeleteSetVariable(r.Context(), actor(r), set, id); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}

// attachVariableSet applies the set to exactly the workspaces (by name) in the workspace fields.
func (s *Server) attachVariableSet(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	set := r.PathValue("set")
	err := s.App.SelectSetWorkspaces(r.Context(), actor(r), set, f["workspace"])
	if s.failed(w, r, err, "workspaces", f, s.onSetPage(w, r)) {
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}

// attachVariableSetProjects applies the set to exactly the projects (by name) in the project fields.
func (s *Server) attachVariableSetProjects(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	set := r.PathValue("set")
	err := s.App.SelectSetProjects(r.Context(), actor(r), set, f["project"])
	if s.failed(w, r, err, "projects", f, s.onSetPage(w, r)) {
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}
