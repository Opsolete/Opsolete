package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gocloud.dev/gcerrors"

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
	fieldModule    = "ops-module" // name of a template added to a workspace
)

var workspaceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

const workspaceNameRule = "2–63 lowercase letters, digits and dashes, starting with a letter or digit (not \"new\")"

// validWorkspaceName: "new" is reserved for /workspaces/new.
func validWorkspaceName(name string) bool { return workspaceNameRe.MatchString(name) && name != "new" }

func (s *Server) workspaceRoutes(mux *http.ServeMux) {
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
	for _, tab := range []string{"runs", "states", "variables", "settings"} {
		mux.HandleFunc("GET /workspaces/{name}/"+tab, s.workspace)
	}
	mux.HandleFunc("GET /workspaces/{name}/settings/{page}", s.workspace)
	mux.HandleFunc("POST /workspaces/{name}/runs", s.newRun)
	mux.HandleFunc("POST /workspaces/{name}/settings", s.workspaceSettings)
	mux.HandleFunc("GET /workspaces/{name}/configure", s.configureForm)
	mux.HandleFunc("POST /workspaces/{name}/configure", s.configureWorkspace)
	mux.HandleFunc("POST /workspaces/{name}/delete", s.deleteWorkspace)
	mux.HandleFunc("GET /workspaces/{name}/templates/new", s.workspaceTemplateForm)
	mux.HandleFunc("POST /workspaces/{name}/templates/new", s.saveWorkspaceTemplate)
	mux.HandleFunc("GET /workspaces/{name}/templates/{module}", s.workspaceTemplateForm)
	mux.HandleFunc("POST /workspaces/{name}/templates/{module}", s.saveWorkspaceTemplate)
	mux.HandleFunc("POST /workspaces/{name}/templates/{module}/remove", s.removeWorkspaceTemplate)
	mux.HandleFunc("POST /workspaces/{name}/access", s.admin(s.workspaceAccess))
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}", s.run)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/confirm", s.confirmRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/discard", s.discardRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/cancel", s.cancelRun)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs", s.logText)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs/stream", s.logStream)
}

func viewer(r *http.Request) storage.Viewer {
	u := auth.FromContext(r.Context())
	return storage.Viewer{UserID: u.ID, Admin: u.Admin()}
}

// Access levels in a workspace: admins and its creator operate it (runs, confirm, destroy, settings,
// rename), groups get read or operate per workspace. Without access a workspace doesn't exist (404).
const (
	levelRead    = "read"
	levelOperate = "operate"
)

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

type formPage struct {
	T         templates.Template
	Editor    bool
	Values    map[string]string
	Errors    map[string]string
	Workspace string
	AutoApply bool
	Configure *storage.Workspace // set: change this workspace's inputs and module version instead of creating one
	Module    string             // with Configure: the added template to change (or add, with Adding)
	Adding    bool
	Cur       templates.Module // with Configure: the module version it runs now
	Error     string
}

func (s *Server) form(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Templates.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	vals := map[string]string{}
	for _, in := range t.Inputs {
		vals[in.Name] = in.Default
	}
	s.render(w, r, http.StatusOK, "form.html", formPage{T: t, Editor: s.canEdit(r), Values: vals})
}

// createWorkspace creates a workspace from a template and queues its first plan.
func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Templates.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := map[string]string{}
	for _, in := range t.Inputs {
		raw[in.Name] = r.PostForm.Get(in.Name)
	}
	vals, errs := t.Validate(raw)
	if errs == nil {
		errs = map[string]string{}
	}
	name := strings.TrimSpace(r.PostForm.Get(fieldWorkspace))
	if !validWorkspaceName(name) {
		errs[fieldWorkspace] = workspaceNameRule
	}
	page := formPage{T: t, Editor: s.canEdit(r), Values: raw, Errors: errs, Workspace: name,
		AutoApply: r.PostForm.Get(fieldAutoApply) != ""}
	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	}
	u := auth.FromContext(r.Context())
	ws, run, err := s.Store.CreateWorkspace(r.Context(), name, t.ID, t.Module.Source, t.Module.Ref, vals, page.AutoApply, u.ID)
	if errors.Is(err, storage.ErrNameTaken) {
		errs[fieldWorkspace] = err.Error()
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	}
	if err != nil {
		s.internalError(w, "create workspace", err)
		return
	}
	s.Log.Info("workspace created", "workspace", ws.Name, "template", t.ID, "run", run.ID, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListWorkspaces(r.Context(), viewer(r))
	if err != nil {
		s.internalError(w, "list workspaces", err)
		return
	}
	s.render(w, r, http.StatusOK, "workspaces.html", list)
}

// loadWorkspace loads the {name} workspace if the user has at least the given access level; it
// writes a 404 (no access looks like no workspace), 403 or 500 and returns ok=false otherwise.
func (s *Server) loadWorkspace(w http.ResponseWriter, r *http.Request, need string) (ws storage.Workspace, level string, ok bool) {
	ws, err := s.Store.GetWorkspace(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return ws, "", false
	}
	if err != nil {
		s.internalError(w, "get workspace", err)
		return ws, "", false
	}
	if level, err = s.accessLevel(r, ws); err != nil {
		s.internalError(w, "workspace access", err)
		return ws, "", false
	}
	switch {
	case level == "":
		http.NotFound(w, r)
		return ws, "", false
	case need == levelOperate && level != levelOperate:
		http.Error(w, "you have read-only access to this workspace", http.StatusForbidden)
		return ws, level, false
	}
	return ws, level, true
}

// accessLevel is what the request's user may do in ws: levelOperate, levelRead or "" (nothing).
func (s *Server) accessLevel(r *http.Request, ws storage.Workspace) (string, error) {
	if u := auth.FromContext(r.Context()); !u.Admin() {
		return s.Store.Access(r.Context(), ws.ID, u.ID)
	}
	return levelOperate, nil
}

// loadRun loads the {run} of the {name} workspace; a run of another workspace is a 404.
func (s *Server) loadRun(w http.ResponseWriter, r *http.Request, need string) (storage.Workspace, storage.Run, []storage.StatusEntry, string, bool) {
	ws, level, ok := s.loadWorkspace(w, r, need)
	if !ok {
		return ws, storage.Run{}, nil, "", false
	}
	run, hist, err := s.Store.GetRun(r.Context(), r.PathValue("run"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return ws, run, nil, "", false
	}
	if err != nil {
		s.internalError(w, "get run", err)
		return ws, run, nil, "", false
	}
	return ws, run, hist, level, true
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	// settings sub-pages; "" is General
	page := r.PathValue("page")
	if !slices.Contains([]string{"", "health", "access", "deletion"}, page) {
		http.NotFound(w, r)
		return
	}
	ws, level, ok := s.loadWorkspace(w, r, levelRead)
	if !ok {
		return
	}
	ctx := r.Context()
	// the side-menu page: each loads only what it shows (the header's data on every one)
	tab, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/workspaces/"+r.PathValue("name")), "/"), "/")
	if tab == "" {
		tab = "overview"
	}
	runs, err := s.Store.Runs(ctx, ws.ID, 25)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	res, err := s.Store.Resources(ctx, ws.ID)
	if err != nil {
		s.internalError(w, "workspace resources", err)
		return
	}
	versions, err := s.Store.StateVersions(ctx, ws.ID, 10)
	if err != nil {
		s.internalError(w, "state versions", err)
		return
	}
	outputs, destroyed, err := s.Store.CurrentOutputs(ctx, ws.ID)
	if err != nil {
		s.internalError(w, "outputs", err)
		return
	}
	var active, assessment *storage.Run
	if len(runs) > 0 && runs[0].Active() {
		active = &runs[0]
	}
	if a, err := s.Store.LatestAssessment(ctx, ws.ID); err == nil {
		assessment = &a
		if a.Active() { // a drift check holds the workspace like any run
			active = assessment
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.internalError(w, "latest assessment", err)
		return
	}
	var current *runItem
	if len(runs) > 0 {
		current = &runItem{runs[0], true}
	}
	t, hasTemplate := s.Templates.Get(ws.TemplateID)
	data := map[string]any{"W": ws, "Runs": runs, "Active": active, "Current": current, "Page": page,
		"HasTemplate": hasTemplate, "Resources": res, "Versions": versions, "Outputs": outputs, "Destroyed": destroyed,
		"Assessment": assessment, "CanOperate": level == levelOperate, "Admin": auth.FromContext(ctx).Admin()}

	switch tab {
	case "overview":
		// the template now pins another module version than the workspace runs.
		if hasTemplate && (t.Module.Source != ws.ModuleSource || t.Module.Ref != ws.ModuleRef) {
			data["Update"] = &t.Module
		}
		added, err := s.Store.WorkspaceTemplates(ctx, ws.ID)
		if err != nil {
			s.internalError(w, "workspace templates", err)
			return
		}
		templateCount := len(added)
		if ws.TemplateID != "" { // the template the workspace was created from counts too
			templateCount++
		}
		updates := map[string]string{} // added template name -> module version its template pins now
		for _, a := range added {
			if t, ok := s.Templates.Get(a.TemplateID); ok && (t.Module.Source != a.ModuleSource || t.Module.Ref != a.ModuleRef) {
				updates[a.Name] = t.Module.Ref
			}
		}
		var latestPlan, latestApply phase
		if len(runs) > 0 {
			_, hist, err := s.Store.GetRun(ctx, runs[0].ID)
			if err != nil {
				s.internalError(w, "latest run", err)
				return
			}
			latestPlan, latestApply = runPhases(runs[0], hist)
		}
		metrics, err := s.Store.RunMetrics(ctx, ws.ID)
		if err != nil {
			s.internalError(w, "run metrics", err)
			return
		}
		maps.Copy(data, map[string]any{"Added": added, "TemplateCount": templateCount, "Updates": updates, "Catalog": s.Templates.List(),
			"LatestPlan": latestPlan, "LatestApply": latestApply, "Metrics": metrics, "Show": r.URL.Query().Get("show")})

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

	case "variables": // each applied set with its variables, read-only (sets are edited by admins on their page)
		vars, err := s.varsBlock(ctx, storage.VarOwner{WorkspaceID: ws.ID}, "/workspaces/"+ws.Name, level == levelOperate)
		if err != nil {
			s.internalError(w, "workspace variables", err)
			return
		}
		sets, err := s.Store.WorkspaceVariableSets(ctx, ws.ID)
		if err != nil {
			s.internalError(w, "workspace variable sets", err)
			return
		}
		var setVars []setView
		for _, set := range sets {
			b, err := s.varsBlock(ctx, storage.VarOwner{SetID: set.ID}, "", false)
			if err != nil {
				s.internalError(w, "variable set variables", err)
				return
			}
			setVars = append(setVars, setView{set, b})
		}
		maps.Copy(data, map[string]any{"Variables": vars, "Sets": setVars})

	case "settings":
		grants, err := s.Store.Grants(ctx, ws.ID)
		if err != nil {
			s.internalError(w, "workspace grants", err)
			return
		}
		var groups []string
		if auth.FromContext(ctx).Admin() {
			if groups, err = s.Store.KnownGroups(ctx); err != nil {
				s.internalError(w, "known groups", err)
				return
			}
		}
		// General: workspaces to share the state with, the ones it already does first: they stay
		// selected even if this user can't see them
		stateConsumers := picker{Name: "remote_state_consumers", Label: "Workspaces to share the state with"}
		if page == "" && level == levelOperate {
			cur, err := s.Store.RemoteStateConsumers(ctx, ws.ID)
			if err != nil {
				s.internalError(w, "remote state consumers", err)
				return
			}
			all, err := s.Store.ListWorkspaces(ctx, viewer(r))
			if err != nil {
				s.internalError(w, "list workspaces", err)
				return
			}
			seen := map[string]bool{ws.ID: true}
			for _, c := range cur {
				seen[c.ID] = true
				stateConsumers.Items = append(stateConsumers.Items, pickerItem{c.ID, c.Name, true})
				stateConsumers.Selected++
			}
			for _, o := range all {
				if !seen[o.ID] {
					stateConsumers.Items = append(stateConsumers.Items, pickerItem{o.ID, o.Name, false})
				}
			}
		}
		maps.Copy(data, map[string]any{"Grants": grants, "Groups": groups, "StateConsumers": stateConsumers,
			"StateAddress": s.Registry.PublicURL.JoinPath(runnerapi.StatePrefix, ws.ID).String()})
	}
	s.render(w, r, http.StatusOK, "workspace.html", data)
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
func workspacePicker(field, label string, selected []string, all []storage.Workspace, offer func(storage.Workspace) bool) picker {
	p := picker{Name: field, Label: label, Selected: len(selected)}
	for _, n := range selected {
		p.Items = append(p.Items, pickerItem{n, n, true})
	}
	for _, ws := range all {
		if !slices.Contains(selected, ws.Name) && offer(ws) {
			p.Items = append(p.Items, pickerItem{ws.Name, ws.Name, false})
		}
	}
	return p
}

// syncSelection attaches the names in want that have lacks, then detaches the ones want lacks.
// An unknown workspace name fails (sql.ErrNoRows) before anything changes.
func (s *Server) syncSelection(ctx context.Context, have, want []string, attach func(name string, on bool) error) error {
	var added []string
	for _, n := range want {
		if !slices.Contains(have, n) {
			added = append(added, n)
		}
	}
	if ok, err := s.Store.WorkspacesExist(ctx, added); err != nil {
		return err
	} else if !ok {
		return sql.ErrNoRows
	}
	for _, n := range want {
		if !slices.Contains(have, n) {
			if err := attach(n, true); err != nil {
				return err
			}
		}
	}
	for _, n := range have {
		if !slices.Contains(want, n) {
			if err := attach(n, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// newRun queues a plan: action=apply (re-plan with the workspace's inputs; replace=<address> any
// number of times plans replacing those resources), destroy, refresh (refresh-only) or assess
// (check for drift now).
func (s *Server) newRun(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	action := r.PostFormValue("action")
	if action != "apply" && action != "destroy" && action != "refresh" && action != "assess" {
		http.Error(w, "action must be apply, destroy, refresh or assess", http.StatusBadRequest)
		return
	}
	if !ws.HasConfig() {
		http.Error(w, "this workspace has no configuration to plan: link a repository first", http.StatusConflict)
		return
	}
	replace := r.PostForm["replace"]
	if len(replace) > 0 {
		if action != "apply" {
			http.Error(w, "only apply runs can replace resources", http.StatusBadRequest)
			return
		}
		// Only addresses of the current state: they become Terraform arguments.
		res, err := s.Store.Resources(r.Context(), ws.ID)
		if err != nil {
			s.internalError(w, "workspace resources", err)
			return
		}
		for _, a := range replace {
			if !slices.ContainsFunc(res, func(x storage.Resource) bool { return x.Address == a }) {
				http.Error(w, "not a resource of this workspace: "+a, http.StatusBadRequest)
				return
			}
		}
	}
	run, err := s.Store.CreateRun(r.Context(), ws.ID, action, u.ID, replace)
	if errors.Is(err, storage.ErrRunActive) || errors.Is(err, storage.ErrLocalExecution) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, "create run", err)
		return
	}
	s.Log.Info("run queued", "workspace", ws.Name, "run", run.ID, "action", action, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

// configureForm shows the template's form filled with the workspace's inputs, for its current template
// version: new inputs get their defaults, removed ones are dropped on save.
func (s *Server) configureForm(w http.ResponseWriter, r *http.Request) {
	ws, t, ok := s.loadConfigure(w, r)
	if !ok {
		return
	}
	vals := map[string]string{}
	for _, in := range t.Inputs {
		vals[in.Name] = in.Default
		if v, ok := ws.Inputs[in.Name]; ok {
			vals[in.Name] = fmt.Sprint(v)
		}
	}
	s.render(w, r, http.StatusOK, "form.html", formPage{T: t, Values: vals, Configure: &ws,
		Cur: templates.Module{Source: ws.ModuleSource, Ref: ws.ModuleRef}})
}

// configureWorkspace saves new inputs and the template's module version, and starts a plan.
func (s *Server) configureWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, t, ok := s.loadConfigure(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := map[string]string{}
	for _, in := range t.Inputs {
		raw[in.Name] = r.PostForm.Get(in.Name)
	}
	vals, errs := t.Validate(raw)
	page := formPage{T: t, Values: raw, Errors: errs, Configure: &ws, Cur: templates.Module{Source: ws.ModuleSource, Ref: ws.ModuleRef}}
	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	}
	u := auth.FromContext(r.Context())
	run, err := s.Store.ConfigureWorkspace(r.Context(), ws.ID, t.Module.Source, t.Module.Ref, vals, u.ID)
	if errors.Is(err, storage.ErrRunActive) || errors.Is(err, storage.ErrLocalExecution) {
		page.Error = err.Error()
		s.render(w, r, http.StatusConflict, "form.html", page)
		return
	}
	if err != nil {
		s.internalError(w, "configure workspace", err)
		return
	}
	s.Log.Info("workspace configured", "workspace", ws.Name, "module", t.Module.Source, "ref", t.Module.Ref, "run", run.ID, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

func (s *Server) loadConfigure(w http.ResponseWriter, r *http.Request) (storage.Workspace, templates.Template, bool) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return ws, templates.Template{}, false
	}
	t, ok := s.Templates.Get(ws.TemplateID)
	if !ok {
		http.Error(w, "this workspace's template no longer exists", http.StatusNotFound)
	}
	return ws, t, ok
}

// workspaceSettings saves the General (name, execution mode, auto-apply, remote state sharing) and Health pages. A new name changes
// the URL; state, runs and grants stay (keyed by id).
func (s *Server) workspaceSettings(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" && name != ws.Name {
		if !validWorkspaceName(name) {
			http.Error(w, "a workspace name is "+workspaceNameRule, http.StatusBadRequest)
			return
		}
		if err := s.Store.RenameWorkspace(r.Context(), ws.ID, name); errors.Is(err, storage.ErrNameTaken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		} else if err != nil {
			s.internalError(w, "rename workspace", err)
			return
		}
		s.Log.Info("workspace renamed", "from", ws.Name, "to", name, "user", auth.FromContext(r.Context()).Email)
		ws.Name = name
	}
	if err := s.Store.SetSettings(r.Context(), ws.ID, r.PostFormValue("auto_apply") != "", r.PostFormValue("drift_detection") != ""); err != nil {
		s.internalError(w, "workspace settings", err)
		return
	}
	switch mode := r.PostFormValue("execution_mode"); mode { // General page only
	case "":
	case "remote", "local":
		if err := s.Store.SetExecutionMode(r.Context(), ws.ID, mode); err != nil {
			s.internalError(w, "execution mode", err)
			return
		}
	default:
		http.Error(w, "execution mode is remote or local", http.StatusBadRequest)
		return
	}
	if mode := r.PostFormValue("remote_state"); mode != "" { // General page only
		if err := s.Store.SetRemoteStateSharing(r.Context(), ws.ID, mode == "all", r.PostForm["remote_state_consumers"]); err != nil {
			s.internalError(w, "remote state sharing", err)
			return
		}
	}
	back := "/workspaces/" + ws.Name + "/settings"
	if r.PostFormValue("page") == "health" {
		back += "/health"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// deleteWorkspace deletes a workspace that manages no resources, with its state files.
func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	keys, err := s.Store.DeleteWorkspace(r.Context(), ws.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows): // deleted meanwhile
		http.NotFound(w, r)
		return
	case errors.Is(err, storage.ErrRunActive), errors.Is(err, storage.ErrHasResources):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.internalError(w, "delete workspace", err)
		return
	}
	for _, k := range keys { // failures only leave an unreferenced file behind
		if err := s.StateStore.Delete(r.Context(), k); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
			s.Log.Warn("delete state file", "workspace", ws.Name, "key", k, "err", err)
		}
	}
	s.Log.Info("workspace deleted", "workspace", ws.Name, "id", ws.ID, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces", http.StatusSeeOther)
}

// workspaceAccess grants an IdP group read or operate access to a workspace (level "" revokes).
// Admins only (enforced by the route).
func (s *Server) workspaceAccess(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	group, level := strings.TrimSpace(r.PostFormValue("group")), r.PostFormValue("level")
	if group == "" || len(group) > 256 || (level != "" && level != levelRead && level != levelOperate) {
		http.Error(w, "group is required, and level must be read, operate or empty (revoke)", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetGrant(r.Context(), ws.ID, group, level); err != nil {
		s.internalError(w, "set grant", err)
		return
	}
	s.Log.Info("workspace access", "workspace", ws.Name, "group", group, "level", level, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/settings/access", http.StatusSeeOther)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	ws, run, hist, level, ok := s.loadRun(w, r, levelRead)
	if !ok {
		return
	}
	plan, apply := runPhases(run, hist)
	s.render(w, r, http.StatusOK, "run.html", map[string]any{"W": ws, "R": run, "History": hist,
		"Plan": plan, "Apply": apply, "CanOperate": level == levelOperate})
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

// ago renders t relative to now, HCP style: "just now", "5 minutes ago", "3 days ago".
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
		for i, e := range hist { // the last attempt: a runner whose lease expired re-queues the run
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
	ws, run, _, _, ok := s.loadRun(w, r, levelOperate)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	status, err := s.Store.CancelRun(r.Context(), run.ID, u.Email, r.PostFormValue("force") != "")
	if errors.Is(err, storage.ErrNotCancelable) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, "cancel run", err)
		return
	}
	if status == storage.Canceled {
		s.DeleteArtifacts(r.Context(), run.ID)
	}
	s.Log.Info("run cancel", "workspace", ws.Name, "run", run.ID, "status", status, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

func (s *Server) confirmRun(w http.ResponseWriter, r *http.Request) { s.decideRun(w, r, true) }
func (s *Server) discardRun(w http.ResponseWriter, r *http.Request) { s.decideRun(w, r, false) }

// decideRun confirms (queues the apply of) or discards a plan that waits for confirmation.
func (s *Server) decideRun(w http.ResponseWriter, r *http.Request, confirm bool) {
	ws, run, _, _, ok := s.loadRun(w, r, levelOperate)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	var err error
	if confirm {
		err = s.Store.ConfirmRun(r.Context(), run.ID, u.ID, u.Email)
	} else {
		err = s.Store.DiscardRun(r.Context(), run.ID, u.Email)
	}
	if errors.Is(err, storage.ErrNotConfirmable) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, "decide run", err)
		return
	}
	if !confirm {
		s.DeleteArtifacts(r.Context(), run.ID)
	}
	s.Log.Info("run decided", "workspace", ws.Name, "run", run.ID, "confirmed", confirm, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID, http.StatusSeeOther)
}

func (s *Server) logText(w http.ResponseWriter, r *http.Request) {
	_, run, _, _, ok := s.loadRun(w, r, levelRead)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var after int64
	for {
		chunks, err := s.Store.Logs(r.Context(), run.ID, after, 500)
		if err != nil {
			s.Log.Error("read logs", "err", err)
			return
		}
		for _, c := range chunks {
			io.WriteString(w, c.Chunk)
			after = c.Seq
		}
		if len(chunks) < 500 {
			return
		}
	}
}

var sseNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// logStream follows a run's log as Server-Sent Events. Event ids are log sequence numbers, so a
// reconnecting EventSource resumes via Last-Event-ID. Ends with a "done" event (data: the run's
// status) once no runner works on the run any more: finished, or waiting for confirmation.
// shortcut: polls Postgres every second; switch to LISTEN/NOTIFY if many viewers make that costly.
func (s *Server) logStream(w http.ResponseWriter, r *http.Request) {
	_, run, _, _, ok := s.loadRun(w, r, levelRead)
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
	idle := 0
	for {
		chunks, err := s.Store.Logs(ctx, run.ID, cursor, 200)
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
		if len(chunks) == 0 {
			cur, _, err := s.Store.GetRun(ctx, run.ID)
			if err != nil {
				return
			}
			if !cur.Working() {
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", cur.Status)
				rc.Flush()
				return
			}
			if idle++; idle%15 == 0 {
				io.WriteString(w, ": keepalive\n\n")
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
