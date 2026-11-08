package httpserver

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
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
	mux.HandleFunc("GET /workspaces/{name}", s.workspace)
	// the workspace's side-menu pages: one handler, workspace.html shows the part of the current tab
	for _, tab := range []string{"runs", "states", "variables", "settings", "deploy"} {
		mux.HandleFunc("GET /workspaces/{name}/"+tab, s.workspace)
	}
	mux.HandleFunc("GET /workspaces/{name}/settings/{page}", s.workspace)
	mux.HandleFunc("GET /workspaces/{name}/states/{version}", s.workspace)
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
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}", s.run)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/confirm", s.confirmRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/discard", s.discardRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/cancel", s.cancelRun)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs", s.logText)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs/stream", s.logStream)
}

type formPage struct {
	T         templates.Template
	Editor    bool
	Preview   bool // the builder's unsaved template: shown, never submitted
	Values    map[string]string
	F         *formState // a refused submit
	Workspace string
	AutoApply bool
	Project   string             // the project picked for the new workspace; "" is the default
	Projects  []storage.Project  // to pick from, when creating a workspace
	Configure *storage.Workspace // set: change this workspace's inputs and module version instead of creating one
	Module    string             // with Configure: the added template to change (or add, with Adding)
	Adding    bool
	Cur       templates.Module // with Configure: the module version it runs now
	Set       map[string]bool  // sensitive inputs with a stored value: left empty, it's kept
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// NamePlaceholder suggests a workspace name from the template's name ("Windows VM" → windows-vm-prod):
// builder-made templates have UUID ids, which read badly.
func (p formPage) NamePlaceholder() string {
	base := cmp.Or(strings.Trim(nonNameChars.ReplaceAllString(strings.ToLower(p.T.Name), "-"), "-"), "workspace")
	return strings.TrimRight(base[:min(len(base), 58)], "-") + "-prod"
}

func (s *Server) form(w http.ResponseWriter, r *http.Request) {
	t, err := s.App.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	vals, _ := app.FormDefaults(t, nil)
	projects, err := s.creatableProjects(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "form.html", formPage{T: t, Editor: s.canEditTemplates(r), Values: vals,
		Project: r.URL.Query().Get("project"), Projects: projects})
}

// createWorkspace creates a workspace with the template added to it and queues its first plan.
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
		AutoApply: f.Get(fieldAutoApply) != "", TemplateID: t.ID, Inputs: formValues(f, t)}
	ws, run, err := s.App.CreateWorkspace(r.Context(), actor(r), in)
	if s.failed(w, r, err, "workspace", f, func(status int, fs *formState) {
		fs.Labels = inputLabels(t)
		page := formPage{T: t, Editor: s.canEditTemplates(r), Values: in.Inputs, F: fs, Workspace: in.Name, AutoApply: in.AutoApply, Project: in.Project}
		if page.Projects, err = s.creatableProjects(r); err != nil {
			s.internalError(w, "list projects", err)
			return
		}
		s.render(w, r, status, "form.html", page)
	}) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
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
	s.render(w, r, http.StatusOK, "workspaces.html", map[string]any{"Workspaces": list, "Projects": projects, "Project": project, "CanCreate": can})
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

// loadRun loads the {run} of the {name} workspace (app.Run); it writes the error (uiError) and
// returns ok=false otherwise.
func (s *Server) loadRun(w http.ResponseWriter, r *http.Request, need app.Level) (app.RunDetail, bool) {
	id, _ := pathID(r, "run") // not an id: not found
	d, err := s.App.Run(r.Context(), actor(r), r.PathValue("name"), id, need)
	if err != nil {
		s.uiError(w, r, err)
		return d, false
	}
	return d, true
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	s.workspacePage(w, r, http.StatusOK, nil)
}

// onWorkspacePage re-renders a refused form on the {name} workspace's page at sub (e.g.
// "/settings/lock") and settings sub-page page.
func (s *Server) onWorkspacePage(w http.ResponseWriter, r *http.Request, sub, page string) func(int, *formState) {
	return func(status int, f *formState) {
		s.workspacePage(w, at(r, "/workspaces/"+r.PathValue("name")+sub, "page", page), status, f)
	}
}

// workspacePage renders the workspace's side-menu page of the request's path; f: a refused form on it.
func (s *Server) workspacePage(w http.ResponseWriter, r *http.Request, status int, f *formState) {
	// settings sub-pages; "" is General
	page := r.PathValue("page")
	if !slices.Contains([]string{"", "health", "lock", "access", "deletion"}, page) {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// the side-menu page: each loads only what it shows (the header's data on every one)
	tab, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/workspaces/"+r.PathValue("name")), "/"), "/")
	if tab == "" {
		tab = "overview"
	}
	need := app.LevelRead
	if tab == "settings" { // readers see what they need of the settings on the overview
		need = app.LevelOperate
	}
	st, err := s.App.State(ctx, actor(r), r.PathValue("name"), need) // the header shows the state on every page
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	ws, level := st.Workspace, st.Level
	runs, err := st.Runs(ctx, 25)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	res, err := st.Resources(ctx)
	if err != nil {
		s.internalError(w, "workspace resources", err)
		return
	}
	versions, err := st.Versions(ctx, 10)
	if err != nil {
		s.internalError(w, "state versions", err)
		return
	}
	outputs, destroyed, err := st.Outputs(ctx)
	if err != nil {
		s.internalError(w, "outputs", err)
		return
	}
	var active *storage.Run
	if len(runs) > 0 && runs[0].Active() {
		active = &runs[0]
	}
	assessment, err := st.LatestAssessment(ctx)
	if err != nil {
		s.internalError(w, "latest assessment", err)
		return
	}
	if assessment != nil && assessment.Active() { // a drift check holds the workspace like any run
		active = assessment
	}
	var current *runItem
	if len(runs) > 0 {
		current = &runItem{runs[0], true}
	}
	lock, err := st.CurrentLock(ctx)
	if err != nil {
		s.internalError(w, "state lock", err)
		return
	}
	data := map[string]any{"W": ws, "Lock": lock, "Me": auth.FromContext(ctx).ID.String(), "Runs": runs, "Active": active, "Current": current, "Page": page,
		"Resources": res, "Versions": versions, "Outputs": outputs, "Destroyed": destroyed,
		"Assessment": assessment, "CanOperate": level == app.LevelOperate, "Admin": auth.FromContext(ctx).Admin(), "F": f}

	switch tab {
	case "overview":
		added, err := st.Templates(ctx)
		if err != nil {
			s.internalError(w, "workspace templates", err)
			return
		}
		updates := map[string]string{}                 // added template name -> module version its template pins now
		names := map[string]string{}                   // template id -> display name; a deleted template shows its id
		inputs := map[string][]templates.StoredValue{} // added template name -> its inputs as its form asks them
		for _, a := range added {
			t, ok := s.App.Templates.Get(a.TemplateID)
			if ok {
				names[t.ID] = t.Name
			}
			inputs[a.Name] = t.Describe(a.Inputs)
			if ok && (t.Module.Source != a.ModuleSource || t.Module.Ref != a.ModuleRef) {
				updates[a.Name] = t.Module.Ref
			}
		}
		var latestPlan, latestApply phase
		if len(runs) > 0 {
			d, err := s.App.Run(ctx, actor(r), ws.Name, runs[0].ID, app.LevelRead)
			if err != nil {
				s.uiError(w, r, err)
				return
			}
			latestPlan, latestApply = runPhases(runs[0], d.History)
		}
		metrics, err := st.RunMetrics(ctx)
		if err != nil {
			s.internalError(w, "run metrics", err)
			return
		}
		groups := groupResources(res, added, names)
		counts := map[string]int{} // module call ("" = root) -> resources in the current state
		for _, g := range groups {
			call, _, _ := strings.Cut(strings.TrimPrefix(g.Module, "module."), "[")
			counts[call] += len(g.Rows)
		}
		maps.Copy(data, map[string]any{"Added": added, "TemplateCount": len(added), "Updates": updates, "TemplateNames": names, "AddedInputs": inputs,
			"ResourceGroups": groups, "TemplateResources": counts,
			"LatestPlan": latestPlan, "LatestApply": latestApply, "Metrics": metrics, "Show": r.URL.Query().Get("show")})

	case "deploy": // the catalog, each template one click from its add form
		data["Catalog"] = s.App.Templates.List()

	case "runs": // status filters, counted over the runs listed
		filter := r.URL.Query().Get("filter")
		counts, list := map[string]int{"all": len(runs)}, []runItem{}
		for i, run := range runs {
			g := runGroup(run.Status)
			counts[g]++
			if filter == "" || filter == "all" || filter == g {
				list = append(list, runItem{run, i == 0})
			}
		}
		maps.Copy(data, map[string]any{"Filter": filter, "Counts": counts, "RunList": list})

	case "states": // one version: what it changed, and what rolling back to it would change
		if r.PathValue("version") == "" {
			break
		}
		id, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d, err := st.Diff(ctx, id) // operators only
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		maps.Copy(data, map[string]any{"State": d.Version, "Prev": d.Prev, "Changes": d.Changes, "IsCurrent": d.IsCurrent, "RollbackChanges": d.Rollback})

	case "variables": // each applied set with its variables, read-only (sets are edited by admins on their page)
		wv, err := s.App.WorkspaceVariables(ctx, actor(r), ws.Name)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		vars := varsBlock{Base: "/workspaces/" + ws.Name, Vars: wv.Own, CanEdit: level == app.LevelOperate, F: formOf(f, "variable")}
		var setVars []setView
		for _, set := range wv.Sets {
			setVars = append(setVars, setView{set.VariableSet, varsBlock{Vars: set.Vars}})
		}
		maps.Copy(data, map[string]any{"Variables": vars, "Sets": setVars})

	case "settings":
		var acc app.Access
		if page == "access" {
			var err error
			if acc, err = s.App.Access(ctx, actor(r), ws.Name); err != nil {
				s.uiError(w, r, err)
				return
			}
			data["ProjectGrants"] = acc.ProjectGrants
		}
		// General: workspaces to share the state with, the ones it already does first: they stay
		// selected even if this user can't see them
		stateConsumers := picker{Name: "remote_state_consumers", Label: "Workspaces to share the state with"}
		if page == "" {
			cur, err := st.RemoteStateConsumers(ctx)
			if err != nil {
				s.uiError(w, r, err)
				return
			}
			all, err := s.App.ListWorkspaces(ctx, actor(r), "")
			if err != nil {
				s.internalError(w, "list workspaces", err)
				return
			}
			seen := map[uuid.UUID]bool{ws.ID: true}
			for _, c := range cur {
				seen[c.ID] = true
				stateConsumers.Items = append(stateConsumers.Items, pickerItem{c.ID.String(), c.Name, true})
				stateConsumers.Selected++
			}
			for _, o := range all {
				if !seen[o.ID] {
					stateConsumers.Items = append(stateConsumers.Items, pickerItem{o.ID.String(), o.Name, false})
				}
			}
		}
		var projects []storage.Project // General: where an admin can move it
		if page == "" && auth.FromContext(ctx).Admin() {
			var err error
			if projects, err = s.App.Projects(ctx, actor(r)); err != nil {
				s.uiError(w, r, err)
				return
			}
		}
		maps.Copy(data, map[string]any{"Grants": acc.Grants, "Groups": acc.Groups, "StateConsumers": stateConsumers, "Projects": projects,
			"StateAddress": s.App.Registry.PublicURL.JoinPath(runnerapi.StatePrefix, ws.ID.String()).String()})
	}
	s.render(w, r, status, "workspace.html", data)
}

// picker is a searchable multi-select of workspaces: the "picker" template in layout.html.
type picker struct {
	Name, Label string       // the checkboxes' form field, and what they select
	Items       []pickerItem // the selected ones first
	Selected    int
}

type pickerItem struct {
	Value, Name string
	Selected    bool
}

// workspacePicker offers the workspaces by name, the selected ones first: they stay selected
// even if all (a capped list) lacks them.
func workspacePicker(field, label string, selected []string, all []storage.Workspace) picker {
	p := picker{Name: field, Label: label, Selected: len(selected)}
	for _, n := range selected {
		p.Items = append(p.Items, pickerItem{n, n, true})
	}
	for _, ws := range all {
		if !slices.Contains(selected, ws.Name) {
			p.Items = append(p.Items, pickerItem{ws.Name, ws.Name, false})
		}
	}
	return p
}

// resourceGroup is the resources of one top-level module ("" = the root: a VCS branch's), with the template it comes from.
type resourceGroup struct {
	Module, Template, Ref string
	Rows                  []resourceRow
}

type resourceRow struct {
	storage.Resource
	Short    string // the address inside the group's module
	Provider string // e.g. azurerm
}

// groupResources groups resources by top-level module (nested modules stay in their parent's group),
// modules by name and the root last.
func groupResources(res []storage.Resource, added []storage.WorkspaceTemplate, names map[string]string) []resourceGroup {
	var groups []resourceGroup
	idx := map[string]int{}
	for _, r := range res {
		top, _, _ := strings.Cut(r.Module, ".module.")
		i, ok := idx[top]
		if !ok {
			g := resourceGroup{Module: top}
			call, _, _ := strings.Cut(strings.TrimPrefix(top, "module."), "[") // module.vm["a"] is the call vm
			for _, a := range added {
				if top != "" && a.Name == call {
					g.Template, g.Ref = cmp.Or(names[a.TemplateID], a.TemplateID), a.ModuleRef
				}
			}
			i, idx[top] = len(groups), len(groups)
			groups = append(groups, g)
		}
		short := r.Address
		if top != "" {
			short = strings.TrimPrefix(r.Address, top+".")
		}
		groups[i].Rows = append(groups[i].Rows, resourceRow{r, short, path.Base(r.Provider)})
	}
	slices.SortFunc(groups, func(a, b resourceGroup) int {
		switch { // group keys are unique: at most one root
		case a.Module == "":
			return 1
		case b.Module == "":
			return -1
		}
		return cmp.Compare(a.Module, b.Module)
	})
	return groups
}

// newRun queues a plan: action=apply (re-plan with the workspace's inputs; replace=<address> any
// number of times plans replacing those resources), destroy, refresh (refresh-only), assess
// (check for drift now) or plan (plan only: a speculative plan, never applied).
func (s *Server) newRun(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ws, run, err := s.App.StartRun(r.Context(), actor(r), r.PathValue("name"), app.NewRun{Action: f.Get("action"), Replace: f["replace"]})
	if s.failed(w, r, err, "run", f, s.onWorkspacePage(w, r, "", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}

// saveGeneral saves the General page's name, execution mode and auto-apply (app.SaveGeneralSettings).
func (s *Server) saveGeneral(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ws, err := s.App.SaveGeneralSettings(r.Context(), actor(r), r.PathValue("name"), app.GeneralSettings{Name: f.Get("name"),
		ExecutionMode: f.Get("execution_mode"), AutoApply: f.Get("auto_apply") != ""})
	if s.failed(w, r, err, "general", f, s.onWorkspacePage(w, r, "/settings", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/settings", http.StatusSeeOther)
}

// shareState saves the General page's remote state sharing: remote_state=all shares with all
// workspaces, else with the remote_state_consumers (ids) (app.ShareState).
func (s *Server) shareState(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	var ids []uuid.UUID
	for _, v := range f["remote_state_consumers"] {
		if id, ok := parseID(v); ok { // not an id: ignored, as unknown ids are
			ids = append(ids, id)
		}
	}
	if err := s.App.ShareState(r.Context(), actor(r), r.PathValue("name"), f.Get("remote_state") == "all", ids); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings", http.StatusSeeOther)
}

// saveHealth saves the Health page: drift detection (app.SaveHealthSettings).
func (s *Server) saveHealth(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if err := s.App.SaveHealthSettings(r.Context(), actor(r), r.PathValue("name"), f.Get("drift_detection") != ""); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings/health", http.StatusSeeOther)
}

// lockWorkspace locks (action=lock, with an optional reason), unlocks or force-unlocks a workspace,
// from its Locking page (app.State.Lock, Unlock).
func (s *Server) lockWorkspace(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ctx, action := r.Context(), f.Get("action")
	if action != "lock" && action != "unlock" && action != "force-unlock" { // the page's buttons: not user input
		http.Error(w, "action is lock, unlock or force-unlock", http.StatusBadRequest)
		return
	}
	st, err := s.App.State(ctx, actor(r), r.PathValue("name"), app.LevelOperate)
	if err == nil && action == "lock" {
		err = st.Lock(ctx, strings.TrimSpace(f.Get("reason")))
	} else if err == nil {
		err = st.Unlock(ctx, action == "force-unlock")
	}
	if s.failed(w, r, err, "lock", f, s.onWorkspacePage(w, r, "/settings/lock", "lock")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+st.Workspace.Name+"/settings/lock", http.StatusSeeOther)
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

// workspaceAccess grants an IdP group read or operate access to a workspace (level "" revokes). Admins only.
func (s *Server) workspaceAccess(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.App.SetGrant(r.Context(), actor(r), r.PathValue("name"), f.Get("group"), f.Get("level"))
	if s.failed(w, r, err, "access", f, s.onWorkspacePage(w, r, "/settings/access", "access")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings/access", http.StatusSeeOther)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadRun(w, r, app.LevelRead)
	if !ok {
		return
	}
	plan, apply := runPhases(d.Run, d.History)
	s.render(w, r, http.StatusOK, "run.html", map[string]any{"W": d.Workspace, "R": d.Run, "History": d.History,
		"Plan": plan, "Apply": apply, "CanOperate": d.Level == app.LevelOperate})
}

// runItem is a row of the runs page; Current: the workspace's latest run.
type runItem struct {
	storage.Run
	Current bool
}

// runGroup is the runs page filter a status falls under.
func runGroup(status string) string {
	switch status {
	case storage.NeedsConfirmation:
		return "attention"
	case storage.Errored:
		return "errored"
	case storage.PlanQueued, storage.Planning, storage.ApplyQueued, storage.Applying:
		return "running"
	case storage.Applied, storage.NoChanges, storage.Planned:
		return "success"
	}
	return "other" // discarded, canceled: only under All
}

// phase is the plan or apply step of a run as the run page shows it.
type phase struct {
	State      string // queued | running | finished | errored | canceled | pending | discarded; "" never runs
	Start, End time.Time
}

func (p phase) Duration() string {
	if p.Start.IsZero() || p.End.IsZero() {
		return ""
	}
	return humanDuration(p.End.Sub(p.Start))
}

// humanDuration: "" for 0 (nothing measured), else "less than a minute" or e.g. "2m5s".
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return "less than a minute"
	}
	return d.Round(time.Second).String()
}

// ago renders t relative to now: "just now", "5 minutes ago", "3 days ago".
func ago(t time.Time) string {
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(n) + " " + unit + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	case d < 365*24*time.Hour:
		return plural(int(d.Hours()/24/30), "month")
	}
	return plural(int(d.Hours()/24/365), "year")
}

// runPhases reads the plan and apply steps from the run's status history (oldest first).
func runPhases(r storage.Run, hist []storage.StatusEntry) (plan, apply phase) {
	span := func(status string) (p phase, next string) {
		for i, e := range hist { // the last entry wins
			if e.Status == status {
				p, next = phase{Start: e.CreatedAt}, ""
				if i+1 < len(hist) {
					p.End, next = hist[i+1].CreatedAt, hist[i+1].Status
				}
			}
		}
		return
	}
	ended := func(next string) string {
		if next == storage.Errored || next == storage.Canceled {
			return next
		}
		return "finished"
	}
	plan, next := span(storage.Planning)
	switch {
	case plan.Start.IsZero() && r.Status == storage.PlanQueued:
		plan.State = "queued"
	case plan.Start.IsZero(): // ended before a runner picked it up
		plan.State = r.Status
	case next == "":
		plan.State = "running"
	default:
		plan.State = ended(next)
	}
	apply, next = span(storage.Applying)
	switch {
	case !apply.Start.IsZero() && next == "":
		apply.State = "running"
	case !apply.Start.IsZero():
		apply.State = ended(next)
	case r.Status == storage.NeedsConfirmation:
		apply.State = "pending"
	case r.Status == storage.ApplyQueued:
		apply.State = "queued"
	case r.Status == storage.Discarded:
		apply.State = "discarded"
	}
	return
}

// cancelRun cancels a queued run, asks a running one to stop (Terraform still saves state), or with
// force=1 cuts off a runner that doesn't react.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	id, _ := pathID(r, "run") // not an id: not found
	if _, err := s.App.CancelRun(r.Context(), actor(r), r.PathValue("name"), id, f.Get("force") != ""); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/runs/"+id.String(), http.StatusSeeOther)
}

// confirmRun queues the apply of a plan that waits for confirmation.
func (s *Server) confirmRun(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "run") // not an id: not found
	s.decided(w, r, id, s.App.ConfirmRun(r.Context(), actor(r), r.PathValue("name"), id))
}

// discardRun ends a plan that waits for confirmation without applying it.
func (s *Server) discardRun(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "run") // not an id: not found
	s.decided(w, r, id, s.App.DiscardRun(r.Context(), actor(r), r.PathValue("name"), id))
}

func (s *Server) decided(w http.ResponseWriter, r *http.Request, id uuid.UUID, err error) {
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/runs/"+id.String(), http.StatusSeeOther)
}

// runLog opens the {run} log of the {name} workspace (app.RunLog); it writes the error and returns
// ok=false otherwise.
func (s *Server) runLog(w http.ResponseWriter, r *http.Request) (app.RunLog, bool) {
	id, _ := pathID(r, "run") // not an id: not found
	l, err := s.App.RunLog(r.Context(), actor(r), r.PathValue("name"), id)
	if err != nil {
		s.uiError(w, r, err)
		return l, false
	}
	return l, true
}

func (s *Server) logText(w http.ResponseWriter, r *http.Request) {
	l, ok := s.runLog(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var after int64
	for {
		chunks, err := l.Chunks(r.Context(), after, 500)
		if err != nil {
			s.App.Log.Error("read logs", "err", err)
			return
		}
		for _, c := range chunks {
			io.WriteString(w, ansiCodes.ReplaceAllString(c.Chunk, "")) // Terraform's colors are for the run page
			after = c.Seq
		}
		if len(chunks) < 500 {
			return
		}
	}
}

var sseNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// ansiCodes matches the color codes (SGR sequences) in Terraform's output.
var ansiCodes = regexp.MustCompile("\x1b\\[[0-9;]*m")

// logStream follows a run's log as Server-Sent Events. Event ids are log sequence numbers, so a
// reconnecting EventSource resumes via Last-Event-ID. Ends with a "done" event (data: the run's
// status) once no runner works on the run any more: finished, or waiting for confirmation.
// shortcut: polls Postgres every second; switch to LISTEN/NOTIFY if many viewers make that costly.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	l, ok := s.runLog(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // long-lived response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx-style ingresses from buffering the stream
	w.WriteHeader(http.StatusOK)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	idle, ended, sent := 0, false, ""
	for {
		chunks, err := l.Chunks(ctx, cursor, 200)
		if err != nil {
			return
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "id: %d\n", c.Seq)
			// SSE ends a line at \r too: a bare \r in Terraform's output would start a bogus field
			for _, line := range strings.Split(sseNewlines.Replace(c.Chunk), "\n") {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			io.WriteString(w, "\n")
			cursor = c.Seq
		}
		if len(chunks) < 200 {
			cur, err := l.Status(ctx)
			if err != nil {
				return
			}
			if cur.Working() && cur.Status != sent { // lets the page re-render its steps, e.g. queued → applying
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", cur.Status)
				sent = cur.Status
			}
			if len(chunks) == 0 {
				if !cur.Working() {
					if !ended { // its last lines may have landed after the Logs query: read once more
						ended = true
						continue
					}
					fmt.Fprintf(w, "event: done\ndata: %s\n\n", cur.Status)
					rc.Flush()
					return
				}
				if idle++; idle%15 == 0 {
					io.WriteString(w, ": keepalive\n\n")
				}
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
		if len(chunks) == 200 {
			continue // more backlog, don't wait
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
