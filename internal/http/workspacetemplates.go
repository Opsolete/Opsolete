package httpserver

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Templates added to a workspace, any number: each is the module block module.<name> next to the
// workspace's own configuration, planned and applied together in its one state.

// moduleNameRe is a Terraform module call name; "new" is the add route.
var moduleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

const moduleNameRule = "1–63 lowercase letters, digits, dashes and underscores, starting with a letter (not \"new\")"

// loadTemplateForm loads the form to add a template (?template=<id>) to the {name} workspace, or
// to change its added template {module}, filled with defaults or the current inputs.
func (s *Server) loadTemplateForm(w http.ResponseWriter, r *http.Request) (formPage, storage.Workspace, bool) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return formPage{}, ws, false
	}
	page := formPage{Configure: &ws, Module: r.PathValue("module"), Adding: r.PathValue("module") == ""}
	id, inputs := r.URL.Query().Get("template"), map[string]any{}
	if !page.Adding {
		added, err := s.Store.WorkspaceTemplates(r.Context(), ws.ID)
		if err != nil {
			s.internalError(w, "workspace templates", err)
			return page, ws, false
		}
		i := -1
		for j, a := range added {
			if a.Name == page.Module {
				i = j
			}
		}
		if i < 0 {
			http.NotFound(w, r)
			return page, ws, false
		}
		id, inputs = added[i].TemplateID, added[i].Inputs
		page.Cur = templates.Module{Source: added[i].ModuleSource, Ref: added[i].ModuleRef}
	}
	t, ok, err := s.template(r.Context(), id)
	if err != nil {
		s.internalError(w, "template", err)
		return page, ws, false
	}
	if !ok {
		http.Error(w, "no such template", http.StatusNotFound)
		return page, ws, false
	}
	if page.Adding {
		page.Module = t.ID
	}
	page.T, page.Values = t, map[string]string{}
	for _, in := range t.Inputs {
		page.Values[in.Name] = in.Default
		if v, ok := inputs[in.Name]; ok {
			page.Values[in.Name] = fmt.Sprint(v)
		}
	}
	return page, ws, true
}

func (s *Server) workspaceTemplateForm(w http.ResponseWriter, r *http.Request) {
	if page, _, ok := s.loadTemplateForm(w, r); ok {
		s.render(w, r, http.StatusOK, "form.html", page)
	}
}

// saveWorkspaceTemplate adds a template, or saves an added one's inputs and the template's module
// version, and starts a plan.
func (s *Server) saveWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	page, ws, ok := s.loadTemplateForm(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	t := page.T
	for _, in := range t.Inputs {
		page.Values[in.Name] = r.PostForm.Get(in.Name)
	}
	var vals map[string]any
	vals, page.Errors = t.Validate(page.Values)
	if page.Adding {
		page.Module = strings.TrimSpace(r.PostForm.Get(fieldModule))
		if !moduleNameRe.MatchString(page.Module) || page.Module == "new" {
			page.Errors[fieldModule] = moduleNameRule
		}
	}
	if len(page.Errors) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	}
	u := auth.FromContext(r.Context())
	run, err := s.Store.SetWorkspaceTemplate(r.Context(), ws.ID, storage.WorkspaceTemplate{Name: page.Module, TemplateID: t.ID,
		ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals}, page.Adding, u.ID)
	switch {
	case errors.Is(err, storage.ErrExists):
		page.Errors[fieldModule] = "this workspace already has a template named " + page.Module
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	case errors.Is(err, storage.ErrRunActive), errors.Is(err, storage.ErrLocalExecution):
		page.Error = err.Error()
		s.render(w, r, http.StatusConflict, "form.html", page)
		return
	case errors.Is(err, sql.ErrNoRows): // removed meanwhile
		http.NotFound(w, r)
		return
	case err != nil:
		s.internalError(w, "save workspace template", err)
		return
	}
	s.Log.Info("workspace template saved", "workspace", ws.Name, "module", page.Module, "template", t.ID, "added", page.Adding,
		"ref", t.Module.Ref, "run", run.ID, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

// removeWorkspaceTemplate removes an added template and starts the plan that destroys its resources.
func (s *Server) removeWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	run, err := s.Store.RemoveWorkspaceTemplate(r.Context(), ws.ID, r.PathValue("module"), u.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
		return
	case errors.Is(err, storage.ErrRunActive), errors.Is(err, storage.ErrLastConfig), errors.Is(err, storage.ErrLocalExecution):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.internalError(w, "remove workspace template", err)
		return
	}
	s.Log.Info("workspace template removed", "workspace", ws.Name, "module", r.PathValue("module"), "run", run.ID, "user", u.Email)
	if run.ID == "" { // nothing was deployed, so nothing to plan
		http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}
