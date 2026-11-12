package httpserver

import (
	"net/http"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// Form fields of the deploy form besides the template inputs. Input names can't contain "-",
// so these never collide with a template variable.
const (
	fieldWorkspace = "ops-workspace"
	fieldAutoApply = "ops-auto-apply"
	fieldProject   = "ops-project"
)

func (s *Server) workspaceRoutes(mux routes) {
	mux.HandleFunc("GET /templates/{id}", s.form)
	mux.HandleFunc("POST /templates/{id}", s.createWorkspace)
	mux.HandleFunc("GET /workspaces", s.workspaces)
	mux.HandleFunc("GET /workspaces/new", s.newWorkspacePage)
	mux.HandleFunc("POST /workspaces/new", s.createEmptyWorkspace)
	mux.HandleFunc("GET /workspaces/{name}/vcs", s.workspaceVCSPage)
	mux.HandleFunc("GET /workspaces/{name}/vcs/edit", s.workspaceVCSPage)
	mux.HandleFunc("POST /workspaces/{name}/vcs", s.linkWorkspaceVCS)
	mux.HandleFunc("POST /workspaces/{name}/state-versions/{version}/rollback", s.rollbackState)
	mux.HandleFunc("GET /v1/vcs-archives/{file}", s.vcsArchive)
	// the workspace's side-menu pages: workspace.html shows the part of the tab
	mux.HandleFunc("GET /workspaces/{name}", s.workspaceTab("overview"))
	for _, tab := range []string{"runs", "states", "variables", "settings", "deploy"} {
		mux.HandleFunc("GET /workspaces/{name}/"+tab, s.workspaceTab(tab))
	}
	mux.HandleFunc("GET /workspaces/{name}/settings/{page}", s.workspaceTab("settings"))
	mux.HandleFunc("GET /workspaces/{name}/states/{version}", s.workspaceTab("states"))
	mux.HandleFunc("POST /workspaces/{name}/runs", s.newRun)
	mux.HandleFunc("POST /workspaces/{name}/settings", s.saveGeneral)
	mux.HandleFunc("POST /workspaces/{name}/settings/health", s.saveHealth)
	mux.HandleFunc("POST /workspaces/{name}/settings/remote-state", s.shareState)
	mux.HandleFunc("POST /workspaces/{name}/lock", s.lockWorkspace)
	mux.HandleFunc("POST /workspaces/{name}/delete", s.deleteWorkspace)
	mux.HandleFunc("GET /workspaces/{name}/templates/new", s.workspaceTemplateForm)
	mux.HandleFunc("POST /workspaces/{name}/templates/new", s.saveWorkspaceTemplate)
	mux.HandleFunc("GET /workspaces/{name}/templates/{module}", s.workspaceTemplateForm)
	mux.HandleFunc("POST /workspaces/{name}/templates/{module}", s.saveWorkspaceTemplate)
	mux.HandleFunc("POST /workspaces/{name}/templates/{module}/remove", s.removeWorkspaceTemplate)
	mux.HandleFunc("POST /workspaces/{name}/access", s.workspaceAccess)
	mux.HandleFunc("GET /workspaces/{name}/promote", s.promotePage)
	mux.HandleFunc("POST /workspaces/{name}/promote", s.promote)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}", s.run)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/confirm", s.confirmRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/discard", s.discardRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/change", s.changeImport)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/plan-again", s.planImportAgain)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/adopt", s.adoptImport)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/cancel", s.cancelRun)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs", s.logText)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs/stream", s.logStream)
}

// createWorkspace creates a workspace with the template added to it and queues its first plan;
// with refresh (the project picker changed) it only shows the form again for the picked project.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	t, err := s.App.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	in := app.NewWorkspace{Name: strings.TrimSpace(f.Get(fieldWorkspace)), Project: f.Get(fieldProject),
		AutoApply: f.Get(fieldAutoApply) != "", TemplateID: t.ID, Inputs: formValues(f, t, func(n string) string { return s.draft(r, f, n) })}
	form := func(status int, fs *formState) { // the form again, as posted
		if fs != nil {
			fs.Labels = inputLabels(t)
		}
		page := formPage{Page: Page{Section: "templates"}, T: t, Editor: s.canEditTemplates(r), Values: in.Inputs, F: fs, Workspace: in.Name, AutoApply: in.AutoApply, Project: in.Project,
			Drafts: s.drafts(r, in.Inputs, sensitiveNames(t))}
		page.importChoice(r) // as posted
		var err error
		if page.Projects, err = s.creatableProjects(r); err != nil {
			s.internalError(w, "list projects", err)
			return
		}
		if err := s.lockDeploy(r, &page); err != nil {
			s.internalError(w, "environment values", err)
			return
		}
		s.render(w, r, status, "form.html", &page)
	}
	if f.Has("refresh") { // another project picked: its values, nothing created
		form(http.StatusOK, nil)
		return
	}
	ws, run, err := s.App.CreateWorkspace(r.Context(), actor(r), in)
	if s.failed(w, r, err, "workspace", f, form) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}

// workspacesPage is the workspace list (workspaces.html).
type workspacesPage struct {
	Page
	Workspaces []storage.Workspace
	Projects   []storage.Project // to filter by
	Project    string            // the filter, a name; "" all
	CanCreate  bool
}

// workspaces lists the workspaces the user may see; ?project= (a name) filters by project.
func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	list, err := s.App.ListWorkspaces(r.Context(), actor(r), project)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	projects, err := s.App.Projects(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	can, err := s.canCreateWorkspaces(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "workspaces.html", &workspacesPage{Page{Section: "workspaces"}, list, projects, project, can})
}

// loadWorkspace loads the {name} workspace if the user has at least need (app.Workspace); it writes
// the error (uiError) and returns ok=false otherwise.
func (s *Server) loadWorkspace(w http.ResponseWriter, r *http.Request, need app.Level) (storage.Workspace, app.Level, bool) {
	ws, level, err := s.App.Workspace(r.Context(), actor(r), r.PathValue("name"), need)
	if err != nil {
		s.uiError(w, r, err)
		return ws, level, false
	}
	return ws, level, true
}

// deleteWorkspace deletes a workspace that manages no resources, with its state files; force=1
// deletes it anyway, leaving any real resources unmanaged.
func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if err := s.App.DeleteWorkspace(r.Context(), actor(r), r.PathValue("name"), f.Get("force") != ""); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces", http.StatusSeeOther)
}
