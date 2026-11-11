package httpserver

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// A workspace's runs: the run page, starting one and deciding on one. Their logs are logs.go's.

// runPage is a run's page (run.html).
type runPage struct {
	Page
	W           storage.Workspace
	R           storage.Run
	History     []storage.StatusEntry // oldest first
	Plan, Apply phase
	Found       []app.FoundTemplate // what a plan that imports found: operators may take it into the form
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadRun(w, r, app.LevelRead)
	if !ok {
		return
	}
	plan, apply := runPhases(d.Run, d.History)
	page := &runPage{Page: workspaceNav(r.PathValue("name"), "runs", "", d.Level == app.LevelOperate),
		W: d.Workspace, R: d.Run, History: d.History, Plan: plan, Apply: apply}
	if d.Level == app.LevelOperate {
		var err error
		if page.Found, err = s.App.FoundTemplates(r.Context(), d); err != nil {
			s.internalError(w, "found values", err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "run.html", page)
}

// runItem is a row of the runs page; Current: the workspace's latest run.
type runItem struct {
	storage.Run
	Current bool
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

// newRun queues a plan: action=apply (re-plan with the workspace's inputs; replace=<address> any
// number of times plans replacing those resources), destroy, refresh (refresh-only), assess
// (check for drift now) or plan (plan only: a speculative plan, never applied).
func (s *Server) newRun(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ws, run, err := s.App.StartRun(r.Context(), actor(r), r.PathValue("name"), app.NewRun{Action: f.Get("action"), Replace: f["replace"]})
	if s.failed(w, r, err, "run", f, s.onWorkspacePage(w, r, "overview", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
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
