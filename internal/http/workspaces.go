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

func (s *Server) workspaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /templates/{id}", s.form)
	mux.HandleFunc("POST /templates/{id}", s.createWorkspace)
	mux.HandleFunc("GET /workspaces", s.workspaces)
	mux.HandleFunc("GET /workspaces/{name}", s.workspace)
	mux.HandleFunc("POST /workspaces/{name}/runs", s.newRun)
	mux.HandleFunc("POST /workspaces/{name}/settings", s.workspaceSettings)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}", s.run)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/confirm", s.confirmRun)
	mux.HandleFunc("POST /workspaces/{name}/runs/{run}/discard", s.discardRun)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs", s.logText)
	mux.HandleFunc("GET /workspaces/{name}/runs/{run}/logs/stream", s.logStream)
	mux.HandleFunc("GET /resources", s.resources)
}

// mayOperate: the user who created the workspace, or an admin, may run, confirm, destroy and configure it.
func mayOperate(u auth.User, w storage.Workspace) bool {
	return u.Admin() || (w.CreatedBy != "" && w.CreatedBy == u.Email)
}

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
	if !workspaceNameRe.MatchString(name) {
		errs[fieldWorkspace] = "2–63 lowercase letters, digits and dashes, starting with a letter or digit"
	}
	page := formPage{T: t, Editor: s.canEdit(r), Values: raw, Errors: errs, Workspace: name, AutoApply: r.PostForm.Get(fieldAutoApply) != ""}
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
	list, err := s.Store.ListWorkspaces(r.Context())
	if err != nil {
		s.internalError(w, "list workspaces", err)
		return
	}
	s.render(w, r, http.StatusOK, "workspaces.html", list)
}

// loadWorkspace writes a 404 or 500 and returns ok=false when the workspace can't be loaded.
func (s *Server) loadWorkspace(w http.ResponseWriter, r *http.Request) (storage.Workspace, bool) {
	ws, err := s.Store.GetWorkspace(r.Context(), r.PathValue("name"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return ws, false
	}
	if err != nil {
		s.internalError(w, "get workspace", err)
		return ws, false
	}
	return ws, true
}

// loadRun loads the {run} of the {name} workspace; a run of another workspace is a 404.
func (s *Server) loadRun(w http.ResponseWriter, r *http.Request) (storage.Workspace, storage.Run, []storage.StatusEntry, bool) {
	ws, ok := s.loadWorkspace(w, r)
	if !ok {
		return ws, storage.Run{}, nil, false
	}
	run, hist, err := s.Store.GetRun(r.Context(), r.PathValue("run"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return ws, run, nil, false
	}
	if err != nil {
		s.internalError(w, "get run", err)
		return ws, run, nil, false
	}
	return ws, run, hist, true
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadWorkspace(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	runs, err := s.Store.Runs(ctx, ws.ID, 25)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	res, err := s.Store.Resources(ctx, "", ws.ID)
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
	var active *storage.Run
	if len(runs) > 0 && runs[0].Active() {
		active = &runs[0]
	}
	s.render(w, r, http.StatusOK, "workspace.html", map[string]any{"W": ws, "Runs": runs, "Active": active,
		"Resources": res, "Versions": versions, "Outputs": outputs, "Destroyed": destroyed,
		"CanOperate": mayOperate(auth.FromContext(ctx), ws)})
}

// newRun queues a plan: action=apply (re-plan with the workspace's inputs) or action=destroy.
func (s *Server) newRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadWorkspace(w, r)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	if !mayOperate(u, ws) {
		http.Error(w, "only the workspace's creator, or an admin, can start runs", http.StatusForbidden)
		return
	}
	action := r.PostFormValue("action")
	if action != "apply" && action != "destroy" {
		http.Error(w, "action must be apply or destroy", http.StatusBadRequest)
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

func (s *Server) workspaceSettings(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadWorkspace(w, r)
	if !ok {
		return
	}
	if !mayOperate(auth.FromContext(r.Context()), ws) {
		http.Error(w, "only the workspace's creator, or an admin, can change its settings", http.StatusForbidden)
		return
	}
	if err := s.Store.SetAutoApply(r.Context(), ws.ID, r.PostFormValue("auto_apply") != ""); err != nil {
		s.internalError(w, "workspace settings", err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	ws, run, hist, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "run.html", map[string]any{"W": ws, "R": run, "History": hist,
		"CanOperate": mayOperate(auth.FromContext(r.Context()), ws)})
}

func (s *Server) confirmRun(w http.ResponseWriter, r *http.Request) { s.decideRun(w, r, true) }
func (s *Server) discardRun(w http.ResponseWriter, r *http.Request) { s.decideRun(w, r, false) }

// decideRun confirms (queues the apply of) or discards a plan that waits for confirmation.
func (s *Server) decideRun(w http.ResponseWriter, r *http.Request, confirm bool) {
	ws, run, _, ok := s.loadRun(w, r)
	if !ok {
		return
	}
	u := auth.FromContext(r.Context())
	if !mayOperate(u, ws) {
		http.Error(w, "only the workspace's creator, or an admin, can confirm or discard its plans", http.StatusForbidden)
		return
	}
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

type typeCount struct {
	Type  string
	Count int
}

// resources lists everything Form2Stack manages: the resources in each workspace's current state.
func (s *Server) resources(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	list, err := s.Store.Resources(r.Context(), q, "")
	if err != nil {
		s.internalError(w, "list resources", err)
		return
	}
	var types []typeCount // list is ordered by type
	workspaces := map[string]bool{}
	for _, res := range list {
		if n := len(types); n == 0 || types[n-1].Type != res.Type {
			types = append(types, typeCount{Type: res.Type})
		}
		types[len(types)-1].Count++
		workspaces[res.Workspace] = true
	}
	s.render(w, r, http.StatusOK, "resources.html", map[string]any{"Resources": list, "Types": types,
		"Workspaces": len(workspaces), "Q": q})
}

func (s *Server) logText(w http.ResponseWriter, r *http.Request) {
	_, run, _, ok := s.loadRun(w, r)
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
	_, run, _, ok := s.loadRun(w, r)
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
