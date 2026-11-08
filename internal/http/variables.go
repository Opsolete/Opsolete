package httpserver

import (
	"net/http"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// Variables: the workspace's own (operate access) and variable sets (admins); the rules are app's.
func (s *Server) variableRoutes(mux *http.ServeMux) {
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

// parseForm parses the posted form itself, whatever middleware ran before; a 400 if it can't.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// variableInput reads a posted variable.
func variableInput(r *http.Request) app.VariableInput {
	return app.VariableInput{Category: r.PostForm.Get("category"), Key: r.PostForm.Get("key"), Value: r.PostForm.Get("value"),
		Sensitive: r.PostForm.Get("sensitive") != ""}
}

// varsBlock is the data of the "variables" partial.
type varsBlock struct {
	Base    string // URL the add and delete forms post under
	Vars    []app.Variable
	CanEdit bool
}

// setView is a variable set applied to a workspace, with its variables.
type setView struct {
	storage.VariableSet
	Vars varsBlock
}

func (s *Server) setWorkspaceVariable(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	if err := s.App.SetWorkspaceVariable(r.Context(), actor(r), r.PathValue("name"), variableInput(r)); err != nil {
		s.uiError(w, r, err)
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
	sets, err := s.App.VariableSets(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "variablesets.html", sets)
}

func (s *Server) createVariableSet(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	in := app.NewVariableSet{Name: r.PostForm.Get("name"), Description: r.PostForm.Get("description"), Global: r.PostForm.Get("global") != "",
		Project: r.PostForm.Get("project"), Workspace: r.PostForm.Get("workspace")}
	if err := s.App.CreateVariableSet(r.Context(), actor(r), in); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/variable-sets/"+strings.TrimSpace(in.Name), http.StatusSeeOther)
}

func (s *Server) variableSet(w http.ResponseWriter, r *http.Request) {
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
	p := workspacePicker("workspace", "Workspaces to apply the set to", set.Workspaces, all)
	projects, err := s.App.Projects(ctx, a)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	var items []pickerItem
	for _, pr := range projects {
		items = append(items, pickerItem{pr.Name, projectLabel(pr), slices.Contains(set.Projects, pr.Name)})
	}
	vars := varsBlock{Base: "/variable-sets/" + set.Name, Vars: set.Vars, CanEdit: true}
	s.render(w, r, http.StatusOK, "variableset.html", map[string]any{"Set": set.VariableSet, "Variables": vars, "Picker": p, "Projects": items})
}

func (s *Server) updateVariableSet(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	set := r.PathValue("set")
	if err := s.App.UpdateVariableSet(r.Context(), actor(r), set, r.PostForm.Get("description"), r.PostForm.Get("global") != ""); err != nil {
		s.uiError(w, r, err)
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
	if !parseForm(w, r) {
		return
	}
	set := r.PathValue("set")
	if err := s.App.SetSetVariable(r.Context(), actor(r), set, variableInput(r)); err != nil {
		s.uiError(w, r, err)
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
	if !parseForm(w, r) {
		return
	}
	set := r.PathValue("set")
	if err := s.App.SelectSetWorkspaces(r.Context(), actor(r), set, r.PostForm["workspace"]); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}

// attachVariableSetProjects applies the set to exactly the projects (by name) in the project fields.
func (s *Server) attachVariableSetProjects(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	set := r.PathValue("set")
	if err := s.App.SelectSetProjects(r.Context(), actor(r), set, r.PostForm["project"]); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/variable-sets/"+set, http.StatusSeeOther)
}
