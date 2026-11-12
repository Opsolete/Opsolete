package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// TemplateUse is one place a template is used: a template added to a workspace (module.<Module>).
type TemplateUse struct {
	WorkspaceID                           uuid.UUID
	Workspace, ProjectName, ExecutionMode string
	Module                                string
	ModuleSource, ModuleRef               string
	Inputs                                map[string]any
	Follows                               []string  // per-environment inputs that follow the project (WorkspaceTemplate.Follows)
	RunID                                 uuid.UUID // the workspace's active run; uuid.Nil if none
	RunStatus                             string    // its status; "" if none
}

// TemplateUses lists every use of template templateID, by workspace name and module; projectID
// limits them to one project, uuid.Nil lists all.
func (s *Store) TemplateUses(ctx context.Context, templateID string, projectID uuid.UUID) ([]TemplateUse, error) {
	rows, err := s.q.TemplateUses(ctx, db.TemplateUsesParams{TemplateID: templateID, ProjectID: optID(projectID)})
	if err != nil {
		return nil, err
	}
	out := make([]TemplateUse, 0, len(rows))
	for _, r := range rows {
		u := TemplateUse{WorkspaceID: r.WorkspaceID, Workspace: r.Workspace, ProjectName: r.ProjectName, ExecutionMode: r.ExecutionMode,
			Module: r.Module, ModuleSource: r.ModuleSource, ModuleRef: r.ModuleRef, Follows: nilIfEmpty(r.Follows), RunID: r.RunID.UUID}
		if r.RunStatus != nil {
			u.RunStatus = *r.RunStatus
		}
		if err := json.Unmarshal(r.Inputs, &u.Inputs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// TemplateUpgrade moves one use of template TemplateID from the version and
// inputs it was read with (From) to a module version and inputs.
type TemplateUpgrade struct {
	TemplateID, Module, ModuleSource, ModuleRef string
	Inputs                                      map[string]any
	Follows                                     []string
	From                                        TemplateUse
}

// ErrRolloutClosed: the rollout was paused, stopped or finished meanwhile.
var ErrRolloutClosed = errors.New("the rollout is no longer active")

// UpgradeTemplates applies ups, all of one workspace, and queues one apply run for them as part of
// rollout rolloutID, which waits for confirmation even with auto-apply. If a use changed meanwhile
// (ErrNotFound), there is no free workspace (ErrRunActive, ErrLocalExecution) or the rollout is
// no longer active (ErrRolloutClosed; ErrNotFound if it doesn't exist), nothing changes. The uses are a
// new configuration version: if the run ends before its apply (e.g. its plan is discarded), the
// workspace keeps the old one, and a resumed rollout retries them.
func (s *Store) UpgradeTemplates(ctx context.Context, workspaceID uuid.UUID, ups []TemplateUpgrade, createdBy, rolloutID uuid.UUID) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		status, err := q.RolloutStatusForShare(ctx, rolloutID)
		if err != nil {
			return notFound(err)
		}
		if status != "active" {
			return ErrRolloutClosed
		}
		cv, mods, err := currentConfig(ctx, q, workspaceID)
		if err != nil {
			return err
		}
		for _, u := range ups {
			// only if it still runs what the new inputs were made from: a change meanwhile isn't overwritten
			i := slices.IndexFunc(mods, func(m WorkspaceTemplate) bool { return m.Name == u.Module })
			if i < 0 || mods[i].TemplateID != u.TemplateID || !sameTemplates([]WorkspaceTemplate{mods[i]},
				[]WorkspaceTemplate{{Name: u.Module, TemplateID: u.TemplateID, ModuleSource: u.From.ModuleSource, ModuleRef: u.From.ModuleRef,
					Inputs: u.From.Inputs, Follows: u.From.Follows}}) {
				return ErrNotFound
			}
			mods = withModule(mods, WorkspaceTemplate{Name: u.Module, TemplateID: u.TemplateID, ModuleSource: u.ModuleSource,
				ModuleRef: u.ModuleRef, Inputs: u.Inputs, Follows: u.Follows})
		}
		if r, err = queueVersion(ctx, q, workspaceID, cv, mods, SourceRollout, "apply", createdBy); err != nil {
			return err
		}
		return q.SetRunRollout(ctx, db.SetRunRolloutParams{ID: r.ID, RolloutID: rolloutID})
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// Rollout moves the workspaces using a template to ModuleSource/ModuleRef, the template's version
// when it started: at most BatchSize workspaces with an open rollout run at a time (0: no limit).
type Rollout struct {
	ID                                  uuid.UUID
	TemplateID, ModuleSource, ModuleRef string
	ProjectID                           uuid.UUID // uuid.Nil: every project
	ProjectName                         string    // "": every project
	BatchSize                           int
	Status                              string // active | paused | done | stopped
	Message                             string
	CreatedBy                           uuid.UUID // uuid.Nil: unknown (deleted, or none)
	CreatedByEmail                      string
	CreatedAt, ResumedAt                time.Time
}

// Open: the rollout may still queue runs (paused ones once resumed).
func (r Rollout) Open() bool { return r.Status == "active" || r.Status == "paused" }

// toRollout maps a rollout_list row (001_schema.sql).
func toRollout(o db.RolloutList) Rollout {
	return Rollout{ID: o.ID, TemplateID: o.TemplateID, ModuleSource: o.ModuleSource, ModuleRef: o.ModuleRef, ProjectID: o.ProjectID.UUID,
		ProjectName: o.ProjectName, BatchSize: int(o.BatchSize), Status: o.Status, Message: o.Message, CreatedBy: o.CreatedBy.UUID,
		CreatedByEmail: o.CreatedByEmail, CreatedAt: o.CreatedAt, ResumedAt: o.ResumedAt}
}

// CreateRollout starts a rollout; ErrExists if the template has an open one. Its first turn is the
// caller's (ticked_at now): DueRollouts doesn't hand it out meanwhile, so the batch limit holds.
func (s *Store) CreateRollout(ctx context.Context, o Rollout) (Rollout, error) {
	if o.BatchSize < 0 || o.BatchSize > math.MaxInt32 {
		return Rollout{}, fmt.Errorf("batch size %d out of range", o.BatchSize)
	}
	id, err := s.q.CreateRollout(ctx, db.CreateRolloutParams{TemplateID: o.TemplateID, ModuleSource: o.ModuleSource, ModuleRef: o.ModuleRef,
		ProjectID: optID(o.ProjectID), BatchSize: int32(o.BatchSize), CreatedBy: optID(o.CreatedBy)})
	if isUnique(err) {
		return Rollout{}, ErrExists
	}
	if err != nil {
		return Rollout{}, err
	}
	row, err := s.q.GetRollout(ctx, id)
	return toRollout(row), err
}

// LatestRollout returns a template's most recent rollout; ErrNotFound if it never had one.
func (s *Store) LatestRollout(ctx context.Context, templateID string) (Rollout, error) {
	row, err := s.q.LatestRollout(ctx, templateID)
	return toRollout(row), notFound(err)
}

// DueRollouts claims the active rollouts no app instance worked on within every, so instances take turns.
// shortcut: a turn that takes longer than every may overlap the next instance's; the batch limit can then be exceeded once.
func (s *Store) DueRollouts(ctx context.Context, every time.Duration) ([]Rollout, error) {
	rows, err := s.q.DueRollouts(ctx, every.Seconds())
	out := make([]Rollout, 0, len(rows))
	for _, o := range rows {
		out = append(out, toRollout(o))
	}
	return out, err
}

// SetRolloutStatus changes an open rollout's status; reports false if it's no longer open. Resuming
// (active) forgets the run failures before, and its next turn is the caller's, as with CreateRollout.
func (s *Store) SetRolloutStatus(ctx context.Context, id uuid.UUID, status, message string) (bool, error) {
	n, err := s.q.SetRolloutStatus(ctx, db.SetRolloutStatusParams{ID: id, Status: status, Message: message})
	return n == 1, err
}

// RolloutRuns counts a rollout's open runs, and describes the first of its runs that errored, was
// discarded or canceled after since ("" if none).
func (s *Store) RolloutRuns(ctx context.Context, id uuid.UUID, since time.Time) (open int, failed string, err error) {
	r, err := s.q.RolloutRuns(ctx, db.RolloutRunsParams{ID: id, Since: since})
	return int(r.Open), r.Failed, err
}
