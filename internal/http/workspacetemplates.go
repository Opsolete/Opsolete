package httpserver

import (
	"maps"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Templates added to a workspace (app.TemplateForm, SaveTemplate, RemoveTemplate).

// loadTemplateForm loads the form to add a template (?template=<id>) to the {name} workspace, or
// to change its added template {module}, filled with defaults or the current inputs.
func (s *Server) loadTemplateForm(w http.ResponseWriter, r *http.Request) (formPage, bool) {
	f, err := s.App.TemplateForm(r.Context(), actor(r), r.PathValue("name"), r.PathValue("module"), r.URL.Query().Get("template"))
	if err != nil {
		s.uiError(w, r, err)
		return formPage{}, false
	}
	// TemplateForm needs operate: the menu offers the settings
	page := formPage{Page: workspaceNav(r.PathValue("name"), "templates", "", true), T: f.Template, Configure: &f.Workspace, Module: r.PathValue("module"), Adding: f.Current == nil}
	var inputs map[string]any
	if f.Current != nil {
		inputs, page.Cur = f.Current.Inputs, templates.Module{Source: f.Current.ModuleSource, Ref: f.Current.ModuleRef}
	}
	page.Values, page.Set = app.FormDefaults(f.Template, inputs)
	if from := foundParam(r); page.Adding && from != nil { // importing what a scan found: its values
		c, err := s.App.ScanCandidate(r.Context(), actor(r), r.PathValue("name"), *from)
		if err != nil {
			s.uiError(w, r, err)
			return formPage{}, false
		}
		maps.Copy(page.Values, c.Inputs)
		page.Found, page.Placeholder = &c, map[string]bool{}
		for _, in := range c.Placeholder {
			page.Placeholder[in] = true
		}
	}
	if err := s.lock(r, &page, storage.Project{ID: f.Workspace.ProjectID, Name: f.Workspace.ProjectName}); err != nil {
		s.internalError(w, "environment values", err)
		return formPage{}, false
	}
	return page, true
}

func (s *Server) workspaceTemplateForm(w http.ResponseWriter, r *http.Request) {
	if page, ok := s.loadTemplateForm(w, r); ok {
		s.render(w, r, http.StatusOK, "form.html", &page)
	}
}

// saveWorkspaceTemplate adds a template, or saves an added one's inputs and the template's module
// version, and starts a plan.
func (s *Server) saveWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	page, ok := s.loadTemplateForm(w, r)
	if !ok {
		return
	}
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	page.Values = formValues(f, page.T)
	var from *app.Found
	if page.Adding {
		from = foundParam(r)
	}
	_, run, err := s.App.SaveTemplate(r.Context(), actor(r), r.PathValue("name"), r.PathValue("module"), page.T.ID, page.Values, from)
	if s.failed(w, r, err, "template", f, func(status int, fs *formState) {
		fs.Labels, page.F = inputLabels(page.T), fs
		maps.Copy(page.Values, page.Locked) // shown as before, not as posted
		s.render(w, r, status, "form.html", &page)
	}) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+page.Configure.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}

// foundParam is the scan result the add form imports: ?scan=<run>&n=<index>; nil without.
func foundParam(r *http.Request) *app.Found {
	q := r.URL.Query()
	id, err := uuid.Parse(q.Get("scan"))
	n, nerr := strconv.Atoi(q.Get("n"))
	if err != nil || nerr != nil {
		return nil
	}
	return &app.Found{Run: id, N: n}
}

// removeWorkspaceTemplate removes an added template, and starts the plan that destroys its resources.
func (s *Server) removeWorkspaceTemplate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	run, err := s.App.RemoveTemplate(r.Context(), actor(r), name, r.PathValue("module"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if run.ID == uuid.Nil { // nothing was deployed, so nothing to plan
		http.Redirect(w, r, "/workspaces/"+name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/workspaces/"+name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}
