package httpserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// A template rollout moves the workspaces that use a template on another module version to the
// template's: with their stored inputs and the new inputs' defaults, as the update form would
// without edits. Each workspace gets one plan, which waits for confirmation even with auto-apply.
// The rollout works in batches, retries busy workspaces, and pauses when one of its runs fails or
// is discarded (see TickRollouts).

// Rollout states of a use of the template.
const (
	rolloutCurrent    = "current"     // on the template's version
	rolloutReady      = "ready"       // a rollout upgrades it
	rolloutNeedsInput = "needs_input" // a required input has no value: use the workspace's update form
	rolloutBusy       = "busy"        // a run is active: a rollout retries it once the run ends
	rolloutLocal      = "local"       // local execution: runs can't start here
)

// rolloutEvery is how often an active rollout queues its next batch.
const rolloutEvery = 30 * time.Second

type rolloutRow struct {
	storage.TemplateUse
	State   string
	Missing []string // needs_input: the inputs without a value
}

type rolloutPage struct {
	T        templates.Template
	Rows     []rolloutRow
	Pending  int // workspaces a rollout still has to upgrade: ready or busy
	Projects []storage.Project
	Project  string           // the filter, a name; "" all projects
	Rollout  *storage.Rollout // the template's latest rollout
	OpenRuns int              // its open runs
}

// rolloutPlan sorts t's uses into rows, and the upgrades of the ready ones by workspace id.
func (s *Server) rolloutPlan(t templates.Template, uses []storage.TemplateUse) ([]rolloutRow, map[uuid.UUID][]storage.TemplateUpgrade, error) {
	rows, ups := make([]rolloutRow, len(uses)), map[uuid.UUID][]storage.TemplateUpgrade{}
	for i, u := range uses {
		rows[i] = rolloutRow{TemplateUse: u}
		row := &rows[i]
		if u.ModuleSource == t.Module.Source && u.ModuleRef == t.Module.Ref {
			row.State = rolloutCurrent
			continue
		}
		if u.ExecutionMode != "remote" {
			row.State = rolloutLocal
			continue
		}
		raw, _ := formDefaults(t, u.Inputs)
		vals, errs := t.Validate(raw)
		if err := s.App.SealInputs(t, raw, vals, u.Inputs, errs, u.WorkspaceID, u.Module); err != nil {
			return nil, nil, err
		}
		for _, in := range t.Inputs { // in the form's order
			if errs[in.Name] != "" {
				row.Missing = append(row.Missing, in.Name)
			}
		}
		switch {
		case len(row.Missing) > 0:
			row.State = rolloutNeedsInput
		case u.RunID != uuid.Nil:
			row.State = rolloutBusy
		default:
			row.State = rolloutReady
			ups[u.WorkspaceID] = append(ups[u.WorkspaceID], storage.TemplateUpgrade{TemplateID: t.ID, Module: u.Module,
				ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals, From: u})
		}
	}
	return rows, ups, nil
}

// TickRollouts works on the active rollouts that are due; app instances take turns.
func (s *Server) TickRollouts(ctx context.Context) {
	ros, err := s.App.Store.DueRollouts(ctx, rolloutEvery)
	if err != nil {
		s.App.Log.Error("due template rollouts", "err", err)
		return
	}
	for _, ro := range ros {
		if err := s.tickRollout(ctx, ro); err != nil {
			s.App.Log.Error("template rollout", "template", ro.TemplateID, "err", err)
		}
	}
}

// tickRollout queues ro's next batch. It stops ro when its template moved to another version (the
// inputs were checked against this one), pauses it when one of its runs failed or was discarded,
// and ends it when no workspace is left to upgrade and its runs are done.
func (s *Server) tickRollout(ctx context.Context, ro storage.Rollout) error {
	t, err := s.App.Template(ctx, ro.TemplateID)
	if err != nil && !errors.Is(err, app.ErrNotFound) {
		return err
	}
	status := func(st, msg string) error { _, err := s.App.Store.SetRolloutStatus(ctx, ro.ID, st, msg); return err }
	switch {
	case err != nil:
		return status("stopped", "the template was deleted")
	case t.Module.Source != ro.ModuleSource || t.Module.Ref != ro.ModuleRef:
		return status("stopped", "the template moved to "+t.Module.Ref+": start a new rollout")
	}
	open, failed, err := s.App.Store.RolloutRuns(ctx, ro.ID, ro.ResumedAt)
	if err != nil {
		return err
	}
	if failed != "" {
		return status("paused", failed+": check it, then resume (a workspace whose plan ended before its apply is back on its previous version and is retried)")
	}
	uses, err := s.App.Store.TemplateUses(ctx, t.ID, ro.ProjectID)
	if err != nil {
		return err
	}
	rows, ups, err := s.rolloutPlan(t, uses)
	if err != nil {
		return err
	}
	busy, byHand := 0, 0
	for _, r := range rows {
		switch r.State {
		case rolloutBusy:
			busy++
		case rolloutNeedsInput, rolloutLocal:
			byHand++
		}
	}
	if len(ups) == 0 && busy == 0 && open == 0 {
		msg := ""
		if byHand > 0 {
			msg = fmt.Sprintf("%d use(s) need an update by hand", byHand)
		}
		return status("done", msg)
	}
	slots := len(ups)
	if ro.BatchSize > 0 {
		slots = ro.BatchSize - open
	}
	for _, r := range rows { // by workspace name, so batches go in a predictable order
		list, ok := ups[r.WorkspaceID]
		if !ok {
			continue
		}
		if slots <= 0 {
			break
		}
		delete(ups, r.WorkspaceID) // all of its uses go in one run
		run, err := s.App.Store.UpgradeTemplates(ctx, r.WorkspaceID, list, ro.CreatedBy, ro.ID)
		switch {
		case errors.Is(err, storage.ErrRolloutClosed):
			return nil
		case errors.Is(err, storage.ErrRunActive), errors.Is(err, storage.ErrLocalExecution), errors.Is(err, storage.ErrNotFound):
			continue // changed meanwhile: the next turn sees it as it is now
		case err != nil: // the others may still go through
			s.App.Log.Error("template rollout", "workspace", r.Workspace, "err", err)
			continue
		}
		slots--
		for _, up := range list {
			if err := s.App.Store.Audit(ctx, cmp.Or(ro.CreatedByEmail, "rollout"), "template rollout", map[string]any{"rollout": ro.ID,
				"workspace": r.Workspace, "module": up.Module, "template": t.ID, "from": up.From.ModuleRef, "to": up.ModuleRef, "run": run.ID}); err != nil {
				s.App.Log.Error("audit log write failed", "err", err, "workspace", r.Workspace)
			}
		}
		s.App.Log.Info("template rolled out", "workspace", r.Workspace, "template", t.ID, "ref", t.Module.Ref, "run", run.ID, "rollout", ro.ID)
	}
	return nil
}

func (s *Server) loadRolloutTemplate(w http.ResponseWriter, r *http.Request) (templates.Template, bool) {
	t, err := s.App.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		s.uiError(w, r, err)
		return t, false
	}
	return t, true
}

func (s *Server) rolloutPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.loadRolloutTemplate(w, r)
	if !ok {
		return
	}
	page := rolloutPage{T: t, Project: r.URL.Query().Get("project")}
	var err error
	if page.Projects, err = s.App.Store.Projects(ctx, storage.Viewer{Admin: true}); err != nil {
		s.internalError(w, "projects", err)
		return
	}
	var projectID uuid.UUID
	if page.Project != "" {
		p, err := s.App.Store.Project(ctx, page.Project)
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "no such project", http.StatusNotFound)
			return
		} else if err != nil {
			s.internalError(w, "project", err)
			return
		}
		projectID = p.ID
	}
	uses, err := s.App.Store.TemplateUses(ctx, t.ID, projectID)
	if err != nil {
		s.internalError(w, "template uses", err)
		return
	}
	if page.Rows, _, err = s.rolloutPlan(t, uses); err != nil {
		s.internalError(w, "rollout inputs", err)
		return
	}
	seen := map[uuid.UUID]bool{}
	for _, row := range page.Rows {
		if (row.State == rolloutReady || row.State == rolloutBusy) && !seen[row.WorkspaceID] {
			seen[row.WorkspaceID] = true
			page.Pending++
		}
	}
	ro, err := s.App.Store.LatestRollout(ctx, t.ID)
	switch {
	case err == nil:
		page.Rollout = &ro
		if page.OpenRuns, _, err = s.App.Store.RolloutRuns(ctx, ro.ID, ro.ResumedAt); err != nil {
			s.internalError(w, "rollout runs", err)
			return
		}
	case !errors.Is(err, storage.ErrNotFound):
		s.internalError(w, "rollout", err)
		return
	}
	s.render(w, r, http.StatusOK, "rollout.html", page)
}

// startRollout starts a rollout to the version the page showed, in one project or all, and queues
// its first batch right away.
func (s *Server) startRollout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.loadRolloutTemplate(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("source") != t.Module.Source || r.PostFormValue("ref") != t.Module.Ref {
		http.Error(w, "the template moved to "+t.Module.Ref+" meanwhile: reload the page and check again", http.StatusConflict)
		return
	}
	batch, err := strconv.Atoi(cmp.Or(r.PostFormValue("batch"), "0"))
	if err != nil || batch < 0 {
		http.Error(w, "the batch size is a whole number, 0 for all at once", http.StatusBadRequest)
		return
	}
	name, projectID := r.PostFormValue("project"), uuid.Nil
	if name != "" {
		p, err := s.App.Store.Project(ctx, name)
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "no such project", http.StatusBadRequest)
			return
		} else if err != nil {
			s.internalError(w, "project", err)
			return
		}
		projectID = p.ID
	}
	u := auth.FromContext(ctx)
	ro, err := s.App.Store.CreateRollout(ctx, storage.Rollout{TemplateID: t.ID, ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref,
		ProjectID: projectID, BatchSize: batch, CreatedBy: u.ID})
	if errors.Is(err, storage.ErrExists) {
		http.Error(w, "this template's last rollout is still open: stop it first", http.StatusConflict)
		return
	} else if err != nil {
		s.internalError(w, "create rollout", err)
		return
	}
	s.App.Log.Info("template rollout started", "template", t.ID, "ref", t.Module.Ref, "project", name, "batch", batch, "rollout", ro.ID, "user", u.Email)
	if err := s.tickRollout(ctx, ro); err != nil {
		s.App.Log.Error("template rollout", "template", t.ID, "err", err) // the next turn retries
	}
	http.Redirect(w, r, "/templates/"+t.ID+"/rollout?project="+url.QueryEscape(name), http.StatusSeeOther)
}

// rolloutAction pauses, resumes or stops the template's open rollout.
func (s *Server) rolloutAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, ok := s.loadRolloutTemplate(w, r)
	if !ok {
		return
	}
	status, ok := map[string]string{"pause": "paused", "resume": "active", "stop": "stopped"}[r.PathValue("action")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ro, err := s.App.Store.LatestRollout(ctx, t.ID)
	if errors.Is(err, storage.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.internalError(w, "rollout", err)
		return
	}
	u := auth.FromContext(ctx)
	changed, err := s.App.Store.SetRolloutStatus(ctx, ro.ID, status, map[string]string{"paused": "paused by " + u.Email, "stopped": "stopped by " + u.Email}[status])
	if err != nil {
		s.internalError(w, "set rollout status", err)
		return
	}
	if !changed {
		http.Error(w, "the rollout is no longer open", http.StatusConflict)
		return
	}
	if status == "active" {
		if ro, err = s.App.Store.LatestRollout(ctx, t.ID); err == nil { // resumed_at moved
			err = s.tickRollout(ctx, ro)
		}
		if err != nil {
			s.App.Log.Error("template rollout", "template", t.ID, "err", err)
		}
	}
	http.Redirect(w, r, "/templates/"+t.ID+"/rollout?project="+url.QueryEscape(ro.ProjectName), http.StatusSeeOther)
}
