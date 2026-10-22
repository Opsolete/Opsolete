package httpserver

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
)

// Form fields of the deploy form besides the template inputs. Input names can't contain "-",
// so these never collide with a template variable.
const (
	fieldWorkspace = "f2s-workspace"
	fieldAutoApply = "f2s-auto-apply"
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
	mux.HandleFunc("POST /workspaces/{name}/vcs", s.linkWorkspaceVCS)
	mux.HandleFunc("POST /workspaces/{name}/state", s.uploadState)
	mux.HandleFunc("GET /v1/vcs-archives/{file}", s.vcsArchive)
	mux.HandleFunc("GET /workspaces/{name}", s.workspace)
	mux.HandleFunc("POST /workspaces/{name}/runs", s.newRun)
	mux.HandleFunc("POST /workspaces/{name}/settings", s.workspaceSettings)
	mux.HandleFunc("GET /workspaces/{name}/configure", s.configureForm)
	mux.HandleFunc("POST /workspaces/{name}/configure", s.configureWorkspace)
	mux.HandleFunc("POST /workspaces/{name}/rename", s.renameWorkspace)
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
	u := auth.FromContext(r.Context())
	level = levelOperate
	if !u.Admin() {
		if level, err = s.Store.Access(r.Context(), ws.ID, u.ID); err != nil {
			s.internalError(w, "workspace access", err)
			return ws, "", false
		}
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
	ws, level, ok := s.loadWorkspace(w, r, levelRead)
	if !ok {
		return
	}
	ctx := r.Context()
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
	grants, err := s.Store.Grants(ctx, ws.ID)
	if err != nil {
		s.internalError(w, "workspace grants", err)
		return
	}
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
	var groups []string
	if auth.FromContext(ctx).Admin() {
		if groups, err = s.Store.KnownGroups(ctx); err != nil {
			s.internalError(w, "known groups", err)
			return
		}
	}
	var active *storage.Run
	if len(runs) > 0 && runs[0].Active() {
		active = &runs[0]
	}
	// HCP no-code style: the template now pins another module version than the workspace runs.
	var update *templates.Module
	t, hasTemplate := s.Templates.Get(ws.TemplateID)
	if hasTemplate && (t.Module.Source != ws.ModuleSource || t.Module.Ref != ws.ModuleRef) {
		update = &t.Module
	}
	s.render(w, r, http.StatusOK, "workspace.html", map[string]any{"W": ws, "Runs": runs, "Active": active,
		"Update": update, "HasTemplate": hasTemplate,
		"Resources": res, "Versions": versions, "Outputs": outputs, "Destroyed": destroyed, "Grants": grants,
		"Variables": vars, "Sets": sets, "Groups": groups,
		"CanOperate": level == levelOperate, "Admin": auth.FromContext(ctx).Admin()})
}

// newRun queues a plan: action=apply (re-plan with the workspace's inputs) or action=destroy.
func (s *Server) newRun(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	action := r.PostFormValue("action")
	if action != "apply" && action != "destroy" {
		http.Error(w, "action must be apply or destroy", http.StatusBadRequest)
		return
	}
	if !ws.HasConfig() {
		http.Error(w, "this workspace has no configuration to plan: link a repository first", http.StatusConflict)
		return
	}
	run, err := s.Store.CreateRun(r.Context(), ws.ID, action, u.ID)
	if errors.Is(err, storage.ErrRunActive) {
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
	s.render(w, r, http.StatusOK, "form.html", formPage{T: t, Values: vals, Configure: &ws})
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
	page := formPage{T: t, Values: raw, Errors: errs, Configure: &ws}
	if len(errs) > 0 {
		s.render(w, r, http.StatusUnprocessableEntity, "form.html", page)
		return
	}
	u := auth.FromContext(r.Context())
	run, err := s.Store.ConfigureWorkspace(r.Context(), ws.ID, t.Module.Source, t.Module.Ref, vals, u.ID)
	if errors.Is(err, storage.ErrRunActive) {
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

func (s *Server) workspaceSettings(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	if err := s.Store.SetAutoApply(r.Context(), ws.ID, r.PostFormValue("auto_apply") != ""); err != nil {
		s.internalError(w, "workspace settings", err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// renameWorkspace changes the name in URLs and lists; state, runs and grants stay (keyed by id).
func (s *Server) renameWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
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
	http.Redirect(w, r, "/workspaces/"+name, http.StatusSeeOther)
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
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	ws, run, hist, level, ok := s.loadRun(w, r, levelRead)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "run.html", map[string]any{"W": ws, "R": run, "History": hist,
		"CanOperate": level == levelOperate})
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
		s.deleteArtifacts(r, run.ID)
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
		s.deleteArtifacts(r, run.ID)
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
			for _, line := range strings.Split(c.Chunk, "\n") {
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
