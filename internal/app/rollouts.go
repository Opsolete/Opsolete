package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// A template rollout moves the workspaces that use a template on another module version to the
// template's: with their stored inputs and the new inputs' defaults, as the update form would
// without edits. It covers the template's uses in one project, or in all. Each workspace gets one
// plan (all its uses of the template in one run, storage.UpgradeTemplates), which waits for
// confirmation even with auto-apply; a plan that ends without its apply puts the workspace's
// configuration back. The rollout works in batches (at most BatchSize open runs; 0: all at once),
// retries busy workspaces, pauses when one of its runs fails or is discarded, stops when the
// template moves to another version and ends when no workspace is left. Admins only, all of it;
// the batches after the first are queued by TickRollouts in the name of the admin who started it.

// Rollout states of a use of the template.
const (
	RolloutCurrent    = "current"     // on the template's version
	RolloutReady      = "ready"       // a rollout upgrades it
	RolloutNeedsInput = "needs_input" // a required input has no value: use the workspace's update form
	RolloutBusy       = "busy"        // a run is active: a rollout retries it once the run ends
	RolloutLocal      = "local"       // local execution: runs can't start here
)

// rolloutEvery is how often an active rollout queues its next batch.
const rolloutEvery = 30 * time.Second

var (
	errRolloutOpen   = reason{storage.ErrExists, "this template's last rollout is still open: stop it first"}
	errRolloutClosed = reason{storage.ErrRolloutClosed, "the rollout is no longer open"}
)

// RolloutRow is a use of the template (a module of a workspace) and its rollout state.
type RolloutRow struct {
	storage.TemplateUse
	State   string
	Missing []string // RolloutNeedsInput: the inputs without a value, in the form's order
}

// Rollouts is a template's rollout page: its uses in a project (or all) and its latest rollout.
type Rollouts struct {
	Template templates.Template
	Rows     []RolloutRow     // by workspace name
	Pending  int              // workspaces a rollout still has to upgrade: ready or busy
	Rollout  *storage.Rollout // the template's latest rollout; nil if none
	OpenRuns int              // its open runs
}

// rolloutPlan sorts t's uses into rows, and the upgrades of the ready ones by workspace id.
// env: each use's workspace's project values (useEnvironments): an input the project sets isn't missing.
func (a *App) rolloutPlan(t templates.Template, uses []storage.TemplateUse, env map[uuid.UUID]EnvironmentMap) ([]RolloutRow, map[uuid.UUID][]storage.TemplateUpgrade, error) {
	rows, ups := make([]RolloutRow, len(uses)), map[uuid.UUID][]storage.TemplateUpgrade{}
	for i, u := range uses {
		rows[i] = RolloutRow{TemplateUse: u}
		row := &rows[i]
		if u.ModuleSource == t.Module.Source && u.ModuleRef == t.Module.Ref {
			row.State = RolloutCurrent
			continue
		}
		if u.ExecutionMode != "remote" {
			row.State = RolloutLocal
			continue
		}
		raw, _ := FormDefaults(t, u.Inputs)
		locked := env[u.WorkspaceID].Locked(t, u.Inputs) // as a configure form saves it
		raw = lockInputs(raw, locked)
		vals, errs := t.Validate(raw)
		if err := a.SealInputs(t, raw, vals, u.Inputs, errs, u.WorkspaceID, u.Module); err != nil {
			return nil, nil, err
		}
		keepOwn(vals, errs, locked, u.Inputs)
		for _, in := range t.Inputs { // in the form's order
			if errs[in.Name] != "" {
				row.Missing = append(row.Missing, in.Name)
			}
		}
		switch {
		case len(row.Missing) > 0:
			row.State = RolloutNeedsInput
		case u.RunID != uuid.Nil:
			row.State = RolloutBusy
		default:
			row.State = RolloutReady
			ups[u.WorkspaceID] = append(ups[u.WorkspaceID], storage.TemplateUpgrade{TemplateID: t.ID, Module: u.Module,
				ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals, From: u})
		}
	}
	return rows, ups, nil
}

// useEnvironments loads the project values of each use's workspace, for rolloutPlan.
// shortcut: a query per workspace; one per project if rollouts over many workspaces get slow.
func (a *App) useEnvironments(ctx context.Context, uses []storage.TemplateUse) (map[uuid.UUID]EnvironmentMap, error) {
	out := map[uuid.UUID]EnvironmentMap{}
	for _, u := range uses {
		if _, ok := out[u.WorkspaceID]; ok {
			continue
		}
		var err error
		if out[u.WorkspaceID], err = environmentMap(a.Store.WorkspaceEnvironmentValues(ctx, u.WorkspaceID)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// rolloutProject is the id of the named project, uuid.Nil for "" (all projects); ErrNotFound.
func (a *App) rolloutProject(ctx context.Context, actor Actor, name string) (uuid.UUID, error) {
	if name == "" {
		return uuid.Nil, nil
	}
	p, err := a.Project(ctx, actor, name)
	return p.ID, err
}

// Rollouts loads the rollout page of template id for the uses in project ("" all). Admins only;
// ErrNotFound: the template or the project.
func (a *App) Rollouts(ctx context.Context, actor Actor, id, project string) (Rollouts, error) {
	var v Rollouts
	if !actor.Admin {
		return v, errAdminsOnly
	}
	var err error
	if v.Template, err = a.Template(ctx, id); err != nil {
		return v, err
	}
	projectID, err := a.rolloutProject(ctx, actor, project)
	if err != nil {
		return v, err
	}
	uses, err := a.Store.TemplateUses(ctx, v.Template.ID, projectID)
	if err != nil {
		return v, err
	}
	env, err := a.useEnvironments(ctx, uses)
	if err != nil {
		return v, err
	}
	if v.Rows, _, err = a.rolloutPlan(v.Template, uses, env); err != nil {
		return v, err
	}
	seen := map[uuid.UUID]bool{}
	for _, row := range v.Rows {
		if (row.State == RolloutReady || row.State == RolloutBusy) && !seen[row.WorkspaceID] {
			seen[row.WorkspaceID] = true
			v.Pending++
		}
	}
	ro, err := a.Store.LatestRollout(ctx, v.Template.ID)
	switch {
	case err == nil:
		v.Rollout = &ro
		v.OpenRuns, _, err = a.Store.RolloutRuns(ctx, ro.ID, ro.ResumedAt)
	case errors.Is(err, storage.ErrNotFound):
		err = nil
	}
	return v, err
}

// NewRollout is a rollout to start: the template version the admin saw, and its scope.
type NewRollout struct {
	Source, Ref string // the template's module when the page showed it
	Project     string // name; "" all projects
	BatchSize   int    // open runs at a time; 0 all at once
}

// StartRollout starts a rollout of template id and queues its first batch right away. Admins only;
// ErrNotFound: the template; ErrChanged: the template moved to another version meanwhile; a
// conflict while its last rollout is open; ValidationError: batch size, project.
func (a *App) StartRollout(ctx context.Context, actor Actor, id string, in NewRollout) (storage.Rollout, error) {
	var ro storage.Rollout
	if !actor.Admin {
		return ro, errAdminsOnly
	}
	t, err := a.Template(ctx, id)
	if err != nil {
		return ro, err
	}
	if in.Source != t.Module.Source || in.Ref != t.Module.Ref {
		return ro, reason{ErrChanged, "the template moved to " + t.Module.Ref + " meanwhile: reload the page and check again"}
	}
	if in.BatchSize < 0 || in.BatchSize > 10000 {
		return ro, invalid("batch", "the batch size is a whole number, 0 for all at once")
	}
	projectID, err := a.rolloutProject(ctx, actor, in.Project)
	if errors.Is(err, ErrNotFound) {
		return ro, invalid("project", "no such project")
	} else if err != nil {
		return ro, err
	}
	ro, err = a.Store.CreateRollout(ctx, storage.Rollout{TemplateID: t.ID, ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref,
		ProjectID: projectID, BatchSize: in.BatchSize, CreatedBy: actor.UserID})
	if errors.Is(err, storage.ErrExists) {
		return ro, errRolloutOpen
	} else if err != nil {
		return ro, err
	}
	a.Log.Info("template rollout started", "template", t.ID, "ref", t.Module.Ref, "project", in.Project, "batch", in.BatchSize, "rollout", ro.ID, "user", actor.Email)
	if err := a.tickRollout(ctx, ro); err != nil {
		a.Log.Error("template rollout", "template", t.ID, "err", err) // the next turn retries
	}
	return ro, nil
}

// Rollout actions on a template's open rollout.
const (
	RolloutPause  = "pause"
	RolloutResume = "resume" // queues its next batch right away
	RolloutStop   = "stop"   // for good; the plans it queued stay
)

// RolloutAction pauses, resumes or stops template id's open rollout, and returns it. Admins only;
// ErrNotFound: no rollout (or no such action); a conflict when it isn't open.
func (a *App) RolloutAction(ctx context.Context, actor Actor, id, action string) (storage.Rollout, error) {
	var ro storage.Rollout
	if !actor.Admin {
		return ro, errAdminsOnly
	}
	status, ok := map[string]string{RolloutPause: "paused", RolloutResume: "active", RolloutStop: "stopped"}[action]
	if !ok {
		return ro, ErrNotFound
	}
	ro, err := a.Store.LatestRollout(ctx, id)
	if err != nil {
		return ro, err
	}
	changed, err := a.Store.SetRolloutStatus(ctx, ro.ID, status, map[string]string{"paused": "paused by " + actor.Email, "stopped": "stopped by " + actor.Email}[status])
	if err != nil {
		return ro, err
	}
	if !changed {
		return ro, errRolloutClosed
	}
	a.Log.Info("template rollout "+status, "template", id, "rollout", ro.ID, "user", actor.Email)
	if status == "active" {
		if ro, err = a.Store.LatestRollout(ctx, id); err == nil { // resumed_at moved
			err = a.tickRollout(ctx, ro)
		}
		if err != nil {
			a.Log.Error("template rollout", "template", id, "err", err) // the next turn retries
		}
	}
	return ro, nil
}

// TickRollouts works on the active rollouts that are due; app instances take turns.
func (a *App) TickRollouts(ctx context.Context) {
	ros, err := a.Store.DueRollouts(ctx, rolloutEvery)
	if err != nil {
		a.Log.Error("due template rollouts", "err", err)
		return
	}
	for _, ro := range ros {
		if err := a.tickRollout(ctx, ro); err != nil {
			a.Log.Error("template rollout", "template", ro.TemplateID, "err", err)
		}
	}
}

// tickRollout queues ro's next batch, as a system actor in the name of the admin who started it: one
// audit entry per upgraded use. It stops ro when its template moved to another version (the inputs
// were checked against this one), pauses it when one of its runs failed or was discarded, and ends it
// when no workspace is left to upgrade and its runs are done.
func (a *App) tickRollout(ctx context.Context, ro storage.Rollout) error {
	by := Actor{UserID: ro.CreatedBy, Email: cmp.Or(ro.CreatedByEmail, "rollout"), Admin: true}
	t, err := a.Template(ctx, ro.TemplateID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	status := func(st, msg string) error { _, err := a.Store.SetRolloutStatus(ctx, ro.ID, st, msg); return err }
	switch {
	case err != nil:
		return status("stopped", "the template was deleted")
	case t.Module.Source != ro.ModuleSource || t.Module.Ref != ro.ModuleRef:
		return status("stopped", "the template moved to "+t.Module.Ref+": start a new rollout")
	}
	open, failed, err := a.Store.RolloutRuns(ctx, ro.ID, ro.ResumedAt)
	if err != nil {
		return err
	}
	if failed != "" {
		return status("paused", failed+": check it, then resume (a workspace whose plan ended before its apply is back on its previous version and is retried)")
	}
	uses, err := a.Store.TemplateUses(ctx, t.ID, ro.ProjectID)
	if err != nil {
		return err
	}
	env, err := a.useEnvironments(ctx, uses)
	if err != nil {
		return err
	}
	rows, ups, err := a.rolloutPlan(t, uses, env)
	if err != nil {
		return err
	}
	busy, byHand := 0, 0
	for _, r := range rows {
		switch r.State {
		case RolloutBusy:
			busy++
		case RolloutNeedsInput, RolloutLocal:
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
		run, err := a.Store.UpgradeTemplates(ctx, r.WorkspaceID, list, by.UserID, ro.ID)
		switch {
		case errors.Is(err, storage.ErrRolloutClosed):
			return nil
		case errors.Is(err, storage.ErrRunActive), errors.Is(err, storage.ErrLocalExecution), errors.Is(err, storage.ErrNotFound):
			continue // changed meanwhile: the next turn sees it as it is now
		case err != nil: // the others may still go through
			a.Log.Error("template rollout", "workspace", r.Workspace, "err", err)
			continue
		}
		slots--
		for _, up := range list {
			if err := a.Store.Audit(ctx, by.Email, "template rollout", map[string]any{"rollout": ro.ID,
				"workspace": r.Workspace, "module": up.Module, "template": t.ID, "from": up.From.ModuleRef, "to": up.ModuleRef, "run": run.ID}); err != nil {
				a.Log.Error("audit log write failed", "err", err, "workspace", r.Workspace)
			}
		}
		a.Log.Info("template rolled out", "workspace", r.Workspace, "template", t.ID, "ref", t.Module.Ref, "run", run.ID, "rollout", ro.ID)
	}
	return nil
}
