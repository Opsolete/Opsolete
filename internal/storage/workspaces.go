package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/opsolete/opsolete/internal/storage/db"
)

var (
	ErrNameTaken      = errors.New("a workspace with this name already exists")
	ErrRunActive      = errors.New("the workspace already has an active run: confirm or discard it, or wait for it to finish")
	ErrNotConfirmable = errors.New("the run isn't waiting for confirmation")
	ErrNotCancelable  = errors.New("the run can't be canceled: it isn't queued or running")
	ErrForceFirst     = errors.New("cancel it first: force cancel is for a runner that doesn't stop after that")
	ErrHasResources   = errors.New("the workspace's state still manages resources: destroy them first")
	ErrStateLocked    = errors.New("the workspace's state is locked: wait for whoever holds the lock, or unlock it")
	ErrLocalExecution = errors.New("the workspace uses local execution: Terraform runs on your machine, so runs can't start here (change it in the workspace's settings)")
)

// Viewer is who asks: admins see everything, others their own workspaces and those granted to
// one of their groups, directly or through the workspace's project (access_level).
type Viewer struct {
	UserID uuid.UUID // uuid.Nil: nobody logged in
	Admin  bool
}

// Workspace is a long-lived home for one Terraform state. Its configuration is the templates added
// to it (WorkspaceTemplates), next to the root configuration of a VCS branch (VCS*) or none: an
// empty workspace may hold just variables.
type Workspace struct {
	ID                uuid.UUID
	Name              string
	VCSConnectionID   uuid.UUID // uuid.Nil if not linked to a repository
	VCSRepo           string
	VCSBranch         string
	VCSTrigger        bool   // a new commit on the branch queues an apply run (see app.App.TriggerVCSRuns)
	VCSPullRequests   bool   // open pull requests into the branch get speculative plans
	VCSTagsRegex      string // tag-based: new tags matching it queue runs instead of branch commits; "" branch-based
	VCSTagBaseline    string // commit of the newest matching tag when tag triggers were set: never queued
	Templates         int    // how many templates were added (see WorkspaceTemplates)
	AutoApply         bool   // apply plans of apply runs without confirmation
	DriftDetection    bool   // queue an assess run (refresh-only plan, never applied) daily
	GlobalRemoteState bool   // every workspace's runs may read this state; else only RemoteStateConsumers
	ExecutionMode     string // remote: runs on Opsolete runners; local: the CLI (cloud block) runs Terraform, no runs
	Drifted           bool   // the latest assessment found drift and nothing was applied since
	CreatedBy         string // email; empty for workspaces from before login
	CreatedAt         time.Time
	ProjectID         uuid.UUID
	ProjectName       string
	Latest            *Run // most recent run
}

// VCSLinked: the workspace is linked to a repository branch.
func (w Workspace) VCSLinked() bool { return w.VCSConnectionID != uuid.Nil }

// HasConfig: there is Terraform configuration to plan, so runs can start.
func (w Workspace) HasConfig() bool {
	return w.VCSLinked() || w.Templates > 0
}

// toWorkspace maps a workspace_list row (001_schema.sql).
func toWorkspace(w db.WorkspaceList) Workspace {
	return Workspace{ID: w.ID, Name: w.Name, VCSConnectionID: w.VcsConnectionID.UUID, VCSRepo: w.VcsRepo, VCSBranch: w.VcsBranch,
		VCSTrigger: w.VcsTrigger, VCSPullRequests: w.VcsPullRequests, VCSTagsRegex: w.VcsTagsRegex, VCSTagBaseline: w.VcsTagBaseline,
		Templates: int(w.Templates), AutoApply: w.AutoApply, DriftDetection: w.DriftDetection, GlobalRemoteState: w.GlobalRemoteState,
		ExecutionMode: w.ExecutionMode, Drifted: w.Drifted, CreatedBy: w.CreatedByEmail, CreatedAt: w.CreatedAt,
		ProjectID: w.ProjectID, ProjectName: w.ProjectName}
}

// toRun maps a run_list row (001_schema.sql).
func toRun(r db.RunList) (Run, error) {
	run := Run{ID: r.ID, WorkspaceID: r.WorkspaceID, Action: r.Action, Status: r.Status, Runner: r.Runner,
		CreatedBy: r.CreatedByEmail, ConfirmedBy: r.ConfirmedByEmail, CancelRequested: r.CancelRequested,
		VCSCommit: r.VcsCommit, VCSPullRequest: int(r.VcsPullRequest), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		ConfirmReason: r.ConfirmReason}
	if r.LogBytes > 0 { // retention marks every ended run it passed (so it isn't picked again); only one with a log lost one
		run.LogRemovedAt = r.LogRemovedAt
	}
	for _, f := range []struct {
		raw []byte
		v   any
	}{{r.Plan, &run.Plan}, {r.Replace, &run.Replace}, {r.Outputs, &run.Outputs}} {
		if f.raw != nil {
			if err := json.Unmarshal(f.raw, f.v); err != nil {
				return run, err
			}
		}
	}
	return run, nil
}

// CreateWorkspace creates a workspace with template t added to it and queues its first plan. createdBy
// is a user id, or uuid.Nil. id is the new workspace's id (uuid.Nil = a random one): set when inputs are
// sealed to it beforehand. project is the name of the project it goes into; "" is the default project.
func (s *Store) CreateWorkspace(ctx context.Context, id uuid.UUID, name, project string, t WorkspaceTemplate, autoApply bool, createdBy uuid.UUID) (Workspace, Run, error) {
	return s.createWorkspace(ctx, id, EmptyWorkspace{Name: name, Project: project, AutoApply: autoApply, CreatedBy: createdBy}, &t, "apply")
}

// CreateWorkspaceAs is CreateWorkspace with the first run's action: plan for an import's first round.
func (s *Store) CreateWorkspaceAs(ctx context.Context, id uuid.UUID, name, project string, t WorkspaceTemplate, autoApply bool, action string, createdBy uuid.UUID) (Workspace, Run, error) {
	return s.createWorkspace(ctx, id, EmptyWorkspace{Name: name, Project: project, AutoApply: autoApply, CreatedBy: createdBy}, &t, action)
}

// EmptyWorkspace is a workspace to create without templates and without a run.
type EmptyWorkspace struct {
	Name          string
	Project       string // by name; "" is the default project
	AutoApply     bool
	ExecutionMode string   // "" keeps the default, remote
	VCS           *VCSLink // nil: not linked
	CreatedBy     uuid.UUID
}

// VCSLink is a workspace's repository branch (or tags), as SetWorkspaceVCS takes it.
type VCSLink struct {
	ConnectionID            uuid.UUID
	Repo, Branch, TagsRegex string
	TagBaseline             string
	Trigger, PullRequests   bool
}

// CreateEmptyWorkspace creates a workspace, its execution mode and its VCS link in one transaction:
// if any part fails there is no workspace. ErrNameTaken: the name is in use.
func (s *Store) CreateEmptyWorkspace(ctx context.Context, e EmptyWorkspace) (Workspace, error) {
	w, _, err := s.createWorkspace(ctx, uuid.Nil, e, nil, "")
	return w, err
}

func (s *Store) createWorkspace(ctx context.Context, id uuid.UUID, e EmptyWorkspace, t *WorkspaceTemplate, action string) (Workspace, Run, error) {
	var run Run
	err := s.tx(ctx, func(q *db.Queries) error {
		var err error
		if id, err = insertWorkspace(ctx, q, id, e); err != nil {
			return err
		}
		if t == nil {
			return nil
		}
		// a new workspace starts with the templates it was created with: a discarded first plan leaves them
		cv, err := newVersion(ctx, q, id, uuid.NullUUID{}, []WorkspaceTemplate{*t}, SourceForm, e.CreatedBy)
		if err != nil {
			return err
		}
		if err := setCurrent(ctx, q, id, cv); err != nil {
			return err
		}
		run, err = createRun(ctx, q, id, action, e.CreatedBy)
		return err
	})
	if err != nil {
		return Workspace{}, Run{}, err
	}
	w, err := s.GetWorkspaceByID(ctx, id)
	return w, run, err
}

// insertWorkspace inserts workspace e (id uuid.Nil: a random id) with its execution mode and VCS
// link, and returns its id; ErrNameTaken.
func insertWorkspace(ctx context.Context, q *db.Queries, id uuid.UUID, e EmptyWorkspace) (uuid.UUID, error) {
	id, err := q.InsertWorkspace(ctx, db.InsertWorkspaceParams{ID: optID(id), Name: e.Name, AutoApply: e.AutoApply,
		CreatedBy: optID(e.CreatedBy), Project: e.Project})
	if isUnique(err) {
		return id, ErrNameTaken
	}
	if err != nil {
		return id, err
	}
	if e.ExecutionMode != "" {
		if err := q.SetExecutionMode(ctx, db.SetExecutionModeParams{ID: id, Mode: e.ExecutionMode}); err != nil {
			return id, err
		}
	}
	if l := e.VCS; l != nil {
		if err := q.UpsertWorkspaceVCS(ctx, db.UpsertWorkspaceVCSParams{WorkspaceID: id, ConnectionID: l.ConnectionID, Repo: l.Repo,
			Branch: l.Branch, Trigger: l.Trigger, PullRequests: l.PullRequests, TagsRegex: l.TagsRegex, TagBaseline: l.TagBaseline}); err != nil {
			return id, err
		}
	}
	return id, nil
}

// CreateRun queues a new plan (action apply, destroy, refresh or assess) on a workspace without an
// active run; replace lists resource addresses an apply run replaces.
func (s *Store) CreateRun(ctx context.Context, workspaceID uuid.UUID, action string, createdBy uuid.UUID, replace []string) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		var err error
		if r, err = createRun(ctx, q, workspaceID, action, createdBy); err != nil || len(replace) == 0 {
			return err
		}
		b, err := json.Marshal(replace)
		if err != nil {
			return err
		}
		r.Replace = replace
		return q.SetRunReplace(ctx, db.SetRunReplaceParams{ID: r.ID, Replace: b})
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// createRun queues a run of the workspace's current configuration on a remote workspace: ErrRunActive if it has
// an active run, ErrLocalExecution if it uses local execution (or was deleted meanwhile).
func createRun(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, action string, createdBy uuid.UUID) (Run, error) {
	return queueRun(ctx, q, workspaceID, action, createdBy, uuid.NullUUID{})
}

// queueRun is createRun of configuration version cv (invalid: the current one).
func queueRun(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, action string, createdBy uuid.UUID, cv uuid.NullUUID) (Run, error) {
	r, err := q.QueueRun(ctx, db.QueueRunParams{WorkspaceID: workspaceID, Action: action, CreatedBy: optID(createdBy), CvID: cv,
		Message: action + " plan queued, waiting for a runner"})
	if err == nil {
		if err := notifyJobs(ctx, q); err != nil {
			return Run{}, err
		}
		return Run{ID: r.ID, WorkspaceID: r.WorkspaceID, Action: r.Action, Status: PlanQueued, CreatedAt: r.CreatedAt}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, err
	}
	remote, err := q.WorkspaceIsRemote(ctx, workspaceID)
	switch {
	case err == nil && remote:
		return Run{}, ErrRunActive
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return Run{}, err
	}
	return Run{}, ErrLocalExecution
}

// GetWorkspace returns ErrNotFound for an unknown name.
func (s *Store) GetWorkspace(ctx context.Context, name string) (Workspace, error) {
	w, err := s.q.GetWorkspaceByName(ctx, name)
	return toWorkspace(w), notFound(err)
}

// GetWorkspaceByID returns ErrNotFound for an unknown id.
func (s *Store) GetWorkspaceByID(ctx context.Context, id uuid.UUID) (Workspace, error) {
	w, err := s.q.GetWorkspaceByID(ctx, id)
	return toWorkspace(w), notFound(err)
}

// ListWorkspaces returns the 200 most recently active workspaces v may see, with their latest run;
// project (a name) limits them to one project, "" lists all.
func (s *Store) ListWorkspaces(ctx context.Context, v Viewer, project string) ([]Workspace, error) {
	rows, err := s.q.ListWorkspaces(ctx, db.ListWorkspacesParams{Admin: v.Admin, Viewer: optID(v.UserID), Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]Workspace, 0, len(rows))
	for _, row := range rows {
		w := toWorkspace(row.WorkspaceList)
		if row.LatestStatus != nil {
			w.Latest = &Run{Status: *row.LatestStatus, Action: *row.LatestAction, UpdatedAt: *row.LatestUpdatedAt}
		}
		out = append(out, w)
	}
	return out, nil
}

// WorkspacesExist reports whether there is a workspace for each name.
func (s *Store) WorkspacesExist(ctx context.Context, names []string) (bool, error) {
	return s.q.WorkspacesExist(ctx, names)
}

// DeleteWorkspace deletes a workspace with its runs, variables, grants and state versions, and
// returns the blob keys of its state files for the caller to delete. It refuses while a run is
// active (ErrRunActive) or the current state manages resources or isn't indexed (ErrHasResources),
// so deleting never orphans real infrastructure. force skips the resources check (the state may be
// lost or the resources gone outside Terraform); an active run still refuses.
func (s *Store) DeleteWorkspace(ctx context.Context, id uuid.UUID, force bool) ([]string, error) {
	var keys []string
	err := s.tx(ctx, func(q *db.Queries) error {
		if err := requireIdle(ctx, q, id); err != nil && !(force && errors.Is(err, ErrHasResources)) {
			return err
		}
		// even with force: a CLI's first apply holds the lock but has no state version yet
		locked, err := q.WorkspaceStateLocked(ctx, id)
		if err != nil {
			return notFound(err)
		}
		if locked {
			return ErrStateLocked
		}
		if keys, err = q.WorkspaceBlobKeys(ctx, id); err != nil {
			return err
		}
		return q.DeleteWorkspace(ctx, id)
	})
	return keys, err
}

// requireIdle locks the workspace row (keeping new runs out until the transaction ends: their FK
// check waits) and returns ErrRunActive if a run is active, or ErrHasResources if its current state
// manages resources or isn't indexed; ErrNotFound if there is no such workspace.
func requireIdle(ctx context.Context, q *db.Queries, id uuid.UUID) error {
	w, err := q.LockIdleWorkspace(ctx, id)
	switch {
	case err != nil:
		return notFound(err)
	case w.Active:
		return ErrRunActive
	case w.Deployed:
		return ErrHasResources
	}
	return nil
}

// Access returns what userID may do in a workspace (access_level): "operate", "read" or "". Admins
// are handled by the caller.
func (s *Store) Access(ctx context.Context, workspaceID, userID uuid.UUID) (string, error) {
	return s.q.Access(ctx, db.AccessParams{WorkspaceID: workspaceID, UserID: optID(userID)})
}

// Grant is one group's access to a workspace or project.
type Grant struct {
	Group string
	Level string // read | operate
}

// Grants lists a workspace's group grants.
func (s *Store) Grants(ctx context.Context, workspaceID uuid.UUID) ([]Grant, error) {
	rows, err := s.q.WorkspaceGrants(ctx, workspaceID)
	out := make([]Grant, 0, len(rows))
	for _, g := range rows {
		out = append(out, Grant{Group: g.GroupName, Level: g.Level})
	}
	return out, err
}

// SetGrant gives a group read or operate access to a workspace; level "" removes the grant.
func (s *Store) SetGrant(ctx context.Context, workspaceID uuid.UUID, group, level string) error {
	if level == "" {
		return s.q.DeleteWorkspaceGrant(ctx, db.DeleteWorkspaceGrantParams{WorkspaceID: workspaceID, GroupName: group})
	}
	return s.q.SetWorkspaceGrant(ctx, db.SetWorkspaceGrantParams{WorkspaceID: workspaceID, GroupName: group, Level: level})
}

// SetWorkspaceVCS links a workspace to a repository branch, its root configuration; connID uuid.Nil unlinks it.
// trigger, pullRequests and tagsRegex (tag-based triggers) set which VCS events queue runs; with a
// changed connection, repository or tag format, tagBaseline becomes the tag commit never to queue.
// A changed link or tag format, or turning on pull request plans, detaches the workspace's variable
// sets: an admin must attach them again. It returns ErrRunActive while a run is active: one queued
// now would run another repository's commit.
func (s *Store) SetWorkspaceVCS(ctx context.Context, workspaceID, connID uuid.UUID, repo, branch, tagsRegex, tagBaseline string, trigger, pullRequests bool) error {
	return s.tx(ctx, func(q *db.Queries) error {
		w, err := q.LockWorkspaceVCS(ctx, db.LockWorkspaceVCSParams{WorkspaceID: workspaceID, ConnectionID: optID(connID),
			Repo: repo, Branch: branch, TagsRegex: tagsRegex, PullRequests: pullRequests && connID != uuid.Nil})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted meanwhile: nothing to link
		}
		if err != nil {
			return err
		}
		if w.Active {
			return ErrRunActive
		}
		if connID == uuid.Nil {
			err = q.DeleteWorkspaceVCS(ctx, workspaceID)
		} else {
			err = q.UpsertWorkspaceVCS(ctx, db.UpsertWorkspaceVCSParams{WorkspaceID: workspaceID, ConnectionID: connID, Repo: repo, Branch: branch,
				Trigger: trigger, PullRequests: pullRequests, TagsRegex: tagsRegex, TagBaseline: tagBaseline})
		}
		if err != nil {
			return err
		}
		if err := q.TouchWorkspace(ctx, workspaceID); err != nil {
			return err
		}
		// The admin who attached sets vouched for the old repository and branch, not for the new one,
		// nor for unreviewed pull requests or tags, which may point at any commit.
		if w.Detach {
			return q.DetachWorkspaceSets(ctx, workspaceID)
		}
		return nil
	})
}

// VCSTriggered lists the remote workspaces whose branch pushes or pull requests queue runs.
func (s *Store) VCSTriggered(ctx context.Context) ([]Workspace, error) {
	rows, err := s.q.VCSTriggered(ctx)
	out := make([]Workspace, 0, len(rows))
	for _, w := range rows {
		out = append(out, toWorkspace(w))
	}
	return out, err
}

// QueueVCSRun queues a run of commit, pinned at once: an apply run for a push (pullRequest 0), or a
// speculative plan of a pull request. It queues nothing (false) if the workspace has an active run,
// or a run of this commit exists already: for a push any run but a drift check or speculative plan,
// for a pull request a speculative plan of it. The poller retries at its next round.
func (s *Store) QueueVCSRun(ctx context.Context, workspaceID uuid.UUID, commit string, pullRequest int, message string) (bool, error) {
	action := "apply"
	if pullRequest > 0 {
		action = "plan"
	}
	var e *AuditEntry
	err := s.tx(ctx, func(q *db.Queries) error {
		r, err := q.QueueVCSRun(ctx, db.QueueVCSRunParams{WorkspaceID: workspaceID, Action: action, Commit: commit,
			PullRequest: int32(pullRequest), Message: message})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := notifyJobs(ctx, q); err != nil {
			return err
		}
		entry, err := s.audit(ctx, q, "vcs", "queue run", map[string]any{"principal": map[string]any{"type": "system"},
			"run": r.ID, "workspace_id": workspaceID, "action": action, "commit": commit, "pull_request": pullRequest})
		e = &entry
		return err
	})
	if err != nil || e == nil {
		return false, err
	}
	e.stream()
	return true, nil
}

// PinVCSCommit records the commit a run plans, unless one is recorded already, and returns the
// run's commit: the apply phase gets the commit its plan was made from.
func (s *Store) PinVCSCommit(ctx context.Context, runID uuid.UUID, commit string) (string, error) {
	commit, err := s.q.PinVCSCommit(ctx, db.PinVCSCommitParams{ID: runID, Commit: commit})
	return commit, notFound(err)
}

// General are a workspace's General settings: name (a rename keeps state, runs and grants, keyed
// by id), execution mode ("remote" or "local") and auto-apply.
type General struct {
	Name, ExecutionMode string
	AutoApply           bool
}

// SaveGeneral saves the General settings, and only those. ErrNameTaken: the new name is in use;
// ErrRunActive: the execution mode changes while a run is active (a CLI lock and upload in the gap
// between Terraform releasing the run's lock and the run finishing would get the run's resource
// index); ErrNotFound: no such workspace.
func (s *Store) SaveGeneral(ctx context.Context, id uuid.UUID, g General) error {
	err := s.tx(ctx, func(q *db.Queries) error {
		w, err := q.LockIdleWorkspace(ctx, id) // the row lock keeps a new run out until this commits
		if err != nil {
			return notFound(err)
		}
		if w.Active {
			remote, err := q.WorkspaceIsRemote(ctx, id)
			if err != nil {
				return err
			}
			if remote != (g.ExecutionMode == "remote") {
				return ErrRunActive
			}
		}
		return affected(q.UpdateGeneralSettings(ctx, db.UpdateGeneralSettingsParams{ID: id, Name: g.Name, ExecutionMode: g.ExecutionMode, AutoApply: g.AutoApply}))
	})
	if isUnique(err) {
		return ErrNameTaken
	}
	return err
}

// SetDriftDetection turns the daily drift check on or off (the Health page). ErrNotFound: no such workspace.
func (s *Store) SetDriftDetection(ctx context.Context, id uuid.UUID, on bool) error {
	return affected(s.q.SetDriftDetection(ctx, db.SetDriftDetectionParams{ID: id, DriftDetection: on}))
}

// ShareState sets whom the workspace's state is shared with: all workspaces (global), else the
// consumers (workspace ids; unknown ids and the workspace itself are ignored). All or nothing.
func (s *Store) ShareState(ctx context.Context, id uuid.UUID, global bool, consumers []uuid.UUID) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := affected(q.SetGlobalRemoteState(ctx, db.SetGlobalRemoteStateParams{ID: id, Global: global})); err != nil {
			return err
		}
		if err := q.ClearRemoteStateConsumers(ctx, id); err != nil {
			return err
		}
		return q.AddRemoteStateConsumers(ctx, db.AddRemoteStateConsumersParams{WorkspaceID: id, Consumers: consumers})
	})
}

// SetExecutionMode sets a workspace's execution mode, "remote" or "local". Runs and CLI locks
// already under way finish; the mode only refuses new ones.
func (s *Store) SetExecutionMode(ctx context.Context, workspaceID uuid.UUID, mode string) error {
	return s.q.SetExecutionMode(ctx, db.SetExecutionModeParams{ID: workspaceID, Mode: mode})
}

// RemoteStateConsumers lists the workspaces explicitly allowed to read a workspace's state, by
// name (only ID and Name are set).
func (s *Store) RemoteStateConsumers(ctx context.Context, workspaceID uuid.UUID) ([]Workspace, error) {
	rows, err := s.q.RemoteStateConsumers(ctx, workspaceID)
	out := make([]Workspace, 0, len(rows))
	for _, w := range rows {
		out = append(out, Workspace{ID: w.ID, Name: w.Name})
	}
	return out, err
}

// SharesState reports whether consumerID's runs may read workspaceID's state.
func (s *Store) SharesState(ctx context.Context, workspaceID, consumerID uuid.UUID) (bool, error) {
	return s.q.SharesState(ctx, db.SharesStateParams{WorkspaceID: workspaceID, ConsumerID: consumerID})
}

// QueueAssessments queues an assess run (a refresh-only plan that is never applied) on every
// workspace with drift detection whose current state manages resources and that wasn't assessed
// within every. Workspaces with an active run are skipped; they get their turn at a later call.
func (s *Store) QueueAssessments(ctx context.Context, every time.Duration) (int, error) {
	var n int
	err := s.tx(ctx, func(q *db.Queries) error {
		runs, err := q.QueueAssessments(ctx, every.Seconds())
		if n = len(runs); err != nil || n == 0 {
			return err
		}
		return notifyJobs(ctx, q)
	})
	return n, err
}

// Runs lists a workspace's runs, newest first; assessments (drift checks) are not listed.
func (s *Store) Runs(ctx context.Context, workspaceID uuid.UUID, limit int) ([]Run, error) {
	rows, err := s.q.ListRuns(ctx, db.ListRunsParams{WorkspaceID: workspaceID, Lim: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(rows))
	for _, row := range rows {
		r, err := toRun(row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RunMetrics summarizes a workspace's runs (drift checks excluded) for its overview page.
type RunMetrics struct {
	Runs, Failed      int
	AvgPlan, AvgApply time.Duration // 0 when no plan or apply has finished
}

// RunMetrics: a phase lasts from its planning/applying entry to the next history entry.
func (s *Store) RunMetrics(ctx context.Context, workspaceID uuid.UUID) (RunMetrics, error) {
	m, err := s.q.RunMetrics(ctx, workspaceID)
	return RunMetrics{Runs: int(m.Runs), Failed: int(m.Failed),
		AvgPlan: time.Duration(m.AvgPlan * float64(time.Second)), AvgApply: time.Duration(m.AvgApply * float64(time.Second))}, err
}

// LatestAssessment returns the workspace's most recent assess run; ErrNotFound if there is none.
func (s *Store) LatestAssessment(ctx context.Context, workspaceID uuid.UUID) (Run, error) {
	r, err := s.q.LatestAssessment(ctx, workspaceID)
	if err != nil {
		return Run{}, notFound(err)
	}
	return toRun(r)
}

// CurrentOutputs returns the outputs of the workspace's current state version. destroyed is true
// when the last successful run was a destroy.
func (s *Store) CurrentOutputs(ctx context.Context, workspaceID uuid.UUID) (outputs map[string]any, destroyed bool, err error) {
	row, err := s.q.CurrentOutputs(ctx, workspaceID)
	if err != nil || row.Outputs == nil {
		return nil, row.Destroyed, err
	}
	return outputs, row.Destroyed, json.Unmarshal(row.Outputs, &outputs)
}

// RunStatus is a run with only its status: GetRun without the columns and history, for polling.
// ErrNotFound if the id is unknown.
func (s *Store) RunStatus(ctx context.Context, id uuid.UUID) (Run, error) {
	status, err := s.q.RunStatus(ctx, id)
	return Run{Status: status}, notFound(err)
}

// GetRun returns a run and its status history; ErrNotFound if the id is unknown.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (Run, []StatusEntry, error) {
	row, err := s.q.GetRun(ctx, id)
	if err != nil {
		return Run{}, nil, notFound(err)
	}
	r, err := toRun(row)
	if err != nil {
		return r, nil, err
	}
	rows, err := s.q.RunHistory(ctx, id)
	hist := make([]StatusEntry, 0, len(rows))
	for _, e := range rows {
		hist = append(hist, StatusEntry{Status: e.Status, Message: e.Message, CreatedAt: e.CreatedAt})
	}
	return r, hist, err
}

// ConfirmRun queues the apply of a plan that waits for confirmation.
func (s *Store) ConfirmRun(ctx context.Context, id, userID uuid.UUID, email string) error {
	return s.decide(ctx, id, EvConfirm, ApplyQueued, userID, "confirmed by "+email+", waiting for a runner")
}

// DiscardRun ends a run whose plan waits for confirmation, without applying it.
func (s *Store) DiscardRun(ctx context.Context, id uuid.UUID, email string) error {
	return s.decide(ctx, id, EvDiscard, Discarded, uuid.Nil, "discarded by "+email)
}

// decide moves a run out of needs_confirmation; confirmedBy (a user id) is recorded unless uuid.Nil.
func (s *Store) decide(ctx context.Context, id uuid.UUID, ev Event, status string, confirmedBy uuid.UUID, message string) error {
	return s.tx(ctx, func(q *db.Queries) error {
		// only confirm and discard leave needs_confirmation (the state machine)
		if _, err := setStatus(ctx, q, id, ev, status, message); errors.Is(err, ErrTransition) || errors.Is(err, ErrNotFound) {
			return ErrNotConfirmable
		} else if err != nil {
			return err
		}
		return q.SetConfirmedBy(ctx, db.SetConfirmedByParams{ID: id, ConfirmedBy: optID(confirmedBy)})
	})
}
