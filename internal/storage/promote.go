package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// ErrChanged: what the caller read changed meanwhile (a race): reload and try again.
var ErrChanged = errors.New("changed meanwhile: reload and try again")

// Promotion makes a workspace's templates exactly Templates (Promote).
type Promotion struct {
	WorkspaceID uuid.UUID
	New         *EmptyWorkspace     // non-nil: create the workspace, with id WorkspaceID, first
	Read        []WorkspaceTemplate // its templates as the promotion was worked out from (WorkspaceTemplates)
	Templates   []WorkspaceTemplate // what it gets; its templates not in it are removed
	Source      string              // the source workspace's name, for the runs' history
	CreatedBy   uuid.UUID
	Actor       string // CreatedBy's email, for the audit entry of a chained apply run
}

// pendingPromotion is runs.promotion: the rest of a promotion its first run (a destroy) carries.
type pendingPromotion struct {
	Source    string              `json:"source"`
	Actor     string              `json:"actor"`
	CreatedBy uuid.UUID           `json:"created_by"`
	Templates []WorkspaceTemplate `json:"templates"` // sensitive inputs sealed
}

// PromotionStep is a promotion's apply run, queued once its destroy run (After) succeeded.
type PromotionStep struct {
	Run    Run
	After  uuid.UUID
	Source string
	Actor  string
}

// ModuleIndex is a workspace's current state index per top-level module (module.<name>, nested
// modules included).
type ModuleIndex struct {
	Resources map[string]int
	Indexed   bool // the current state version is indexed (or there is none)
}

// Deployed: module may manage resources: it has some in the index, or the index is missing.
func (m ModuleIndex) Deployed(module string) bool { return !m.Indexed || m.Resources[module] > 0 }

// ModuleIndex reads the workspace's ModuleIndex.
func (s *Store) ModuleIndex(ctx context.Context, workspaceID uuid.UUID) (ModuleIndex, error) {
	return moduleIndex(ctx, s.q, workspaceID)
}

func moduleIndex(ctx context.Context, q *db.Queries, workspaceID uuid.UUID) (ModuleIndex, error) {
	m := ModuleIndex{Resources: map[string]int{}}
	rows, err := q.ModuleResources(ctx, workspaceID)
	if err != nil {
		return m, err
	}
	for _, r := range rows {
		m.Resources[r.Name] = int(r.Resources)
	}
	m.Indexed, err = q.CurrentStateIndexed(ctx, workspaceID)
	return m, err
}

// Removed lists the templates of cur that want (the promoted templates) removes or replaces (the
// module name holds another template there), by name.
func Removed(cur, want []WorkspaceTemplate) []string {
	var out []string
	for _, t := range cur {
		i := slices.IndexFunc(want, func(w WorkspaceTemplate) bool { return w.Name == t.Name })
		if i < 0 || want[i].TemplateID != t.TemplateID {
			out = append(out, t.Name)
		}
	}
	return out
}

// Promote applies p with runs that always wait for confirmation (runs.always_confirm), even with
// auto-apply. If it removes or replaces a deployed template (ModuleIndex.Deployed), a module that
// configures its own providers can't be destroyed once its block is gone: so the first run is a
// destroy targeting every template it removes or replaces, carrying the rest of the promotion, and
// changes nothing else; applied, it queues the apply run (setStatus, continuePromotion). Otherwise
// it is one apply run: every change is in its config_changes, a removal (before: the template,
// after: null) too, so a run that ends before its apply started puts the previous templates back
// (restoreConfig). Nothing changes if the templates aren't Read anymore (ErrChanged), the name is
// taken (ErrNameTaken), a run is active (ErrRunActive) or the workspace uses local execution
// (ErrLocalExecution). It returns the first run.
func (s *Store) Promote(ctx context.Context, p Promotion) (Run, error) {
	var r Run
	err := s.tx(ctx, func(q *db.Queries) error {
		if p.New != nil {
			if _, err := insertWorkspace(ctx, q, p.WorkspaceID, *p.New); err != nil {
				return err
			}
		} else {
			// the row lock keeps out a template removal without a run (RemoveWorkspaceTemplate) until commit
			w, err := q.LockIdleWorkspace(ctx, p.WorkspaceID)
			if err != nil {
				return notFound(err)
			}
			if w.Active {
				return ErrRunActive
			}
		}
		cur, err := workspaceTemplates(ctx, q, p.WorkspaceID)
		if err != nil {
			return err
		}
		if !sameTemplates(cur, p.Read) {
			return ErrChanged
		}
		removed := Removed(cur, p.Templates)
		destroy := false
		if len(removed) > 0 {
			idx, err := moduleIndex(ctx, q, p.WorkspaceID)
			if err != nil {
				return err
			}
			destroy = slices.ContainsFunc(removed, idx.Deployed)
		}
		if !destroy {
			r, err = queuePromotion(ctx, q, p.WorkspaceID, cur, p.Templates, p.CreatedBy)
			return err
		}
		if r, err = createRun(ctx, q, p.WorkspaceID, "destroy", p.CreatedBy); err != nil {
			return err
		}
		pending, err := json.Marshal(pendingPromotion{Source: p.Source, Actor: p.Actor, CreatedBy: p.CreatedBy, Templates: p.Templates})
		if err != nil {
			return err
		}
		if err := q.SetRunRemoveTemplates(ctx, db.SetRunRemoveTemplatesParams{ID: r.ID, RemoveTemplates: removed}); err != nil {
			return err
		}
		if err := q.SetRunPromotion(ctx, db.SetRunPromotionParams{ID: r.ID, Promotion: pending}); err != nil {
			return err
		}
		if err := q.SetRunAlwaysConfirm(ctx, r.ID); err != nil {
			return err
		}
		return addHistory(ctx, q, r.ID, PlanQueued, "Promotion from "+p.Source+": destroys "+strings.Join(removed, ", ")+
			" first; the promotion's apply run follows once this is applied")
	})
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// continuePromotion queues the apply run of the promotion (raw runs.promotion) whose destroy run
// runID succeeded; if no run can start (the workspace moved to local execution meanwhile) the
// promotion is dropped with a note in runID's history.
func continuePromotion(ctx context.Context, q *db.Queries, runID, workspaceID uuid.UUID, status string, raw []byte) (*PromotionStep, error) {
	var p pendingPromotion
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	cur, err := workspaceTemplates(ctx, q, workspaceID)
	if err != nil {
		return nil, err
	}
	r, err := queuePromotion(ctx, q, workspaceID, cur, p.Templates, p.CreatedBy)
	if errors.Is(err, ErrRunActive) || errors.Is(err, ErrLocalExecution) {
		return nil, addHistory(ctx, q, runID, status, "promotion not continued: "+err.Error())
	}
	if err != nil {
		return nil, err
	}
	if err := addHistory(ctx, q, r.ID, PlanQueued, "Promotion from "+p.Source+", after run "+runID.String()); err != nil {
		return nil, err
	}
	return &PromotionStep{Run: r, After: runID, Source: p.Source, Actor: p.Actor}, nil
}

// queuePromotion queues an apply run that waits for confirmation and makes the workspace's
// templates (cur) exactly want, every change in its config_changes.
func queuePromotion(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, cur, want []WorkspaceTemplate, createdBy uuid.UUID) (Run, error) {
	r, err := createRun(ctx, q, workspaceID, "apply", createdBy)
	if err != nil {
		return r, err
	}
	var changes []configChange
	for _, t := range cur {
		if slices.ContainsFunc(want, func(w WorkspaceTemplate) bool { return w.Name == t.Name }) {
			continue
		}
		before, err := configSnapshot(ctx, q, workspaceID, t.Name)
		if err != nil {
			return r, err
		}
		if err := q.DeleteWorkspaceTemplate(ctx, db.DeleteWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: t.Name}); err != nil {
			return r, err
		}
		changes = append(changes, configChange{Module: t.Name, Before: before})
	}
	for _, t := range want {
		before, err := configSnapshot(ctx, q, workspaceID, t.Name)
		if err != nil {
			return r, err
		}
		raw, err := json.Marshal(t.Inputs)
		if err != nil {
			return r, err
		}
		if before == nil {
			err = affected(q.InsertWorkspaceTemplate(ctx, db.InsertWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: t.Name,
				TemplateID: t.TemplateID, ModuleSource: t.ModuleSource, ModuleRef: t.ModuleRef, Inputs: raw}))
		} else {
			err = affected(q.UpdateWorkspaceTemplate(ctx, db.UpdateWorkspaceTemplateParams{WorkspaceID: workspaceID, Name: t.Name,
				TemplateID: t.TemplateID, ModuleSource: t.ModuleSource, ModuleRef: t.ModuleRef, Inputs: raw}))
		}
		if err != nil {
			return r, err
		}
		after, err := configSnapshot(ctx, q, workspaceID, t.Name)
		if err != nil {
			return r, err
		}
		if !bytes.Equal(before, after) { // jsonb's own text: equal configurations read the same
			changes = append(changes, configChange{Module: t.Name, Before: before})
		}
	}
	if err := q.SetRunAlwaysConfirm(ctx, r.ID); err != nil {
		return r, err
	}
	return r, recordConfigChanges(ctx, q, r.ID, workspaceID, changes)
}

// sameTemplates: a and b (both by name) hold the same configurations.
func sameTemplates(a, b []WorkspaceTemplate) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, err1 := json.Marshal(a[i])
		y, err2 := json.Marshal(b[i])
		if err1 != nil || err2 != nil || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}
