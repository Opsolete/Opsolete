package storage

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// Configuration versions (docs/configuration-versions.md): a workspace's templates are an immutable version. A
// change makes a new one from the current version and queues a run on it; the workspace moves to a run's version
// when its apply starts (setStatus), so a run that ends without its apply changes nothing.

// Sources of a version: what made it.
const (
	SourceForm    = "form"    // the add or configure form, a new workspace
	SourcePromote = "promote" // a promotion
	SourceRollout = "rollout" // a template rollout
	SourceImport  = "import"  // an applied import switched its modules' import input off
	SourceRemove  = "remove"  // a template removed
)

// configModules reads version cv's modules by name; none for no version.
func configModules(ctx context.Context, q *db.Queries, cv uuid.NullUUID) ([]WorkspaceTemplate, error) {
	if !cv.Valid {
		return nil, nil
	}
	rows, err := q.ConfigModules(ctx, cv.UUID)
	if err != nil {
		return nil, err
	}
	out := make([]WorkspaceTemplate, 0, len(rows))
	for _, r := range rows {
		t := WorkspaceTemplate{Name: r.Name, TemplateID: r.TemplateID, ModuleSource: r.ModuleSource, ModuleRef: r.ModuleRef}
		if err := json.Unmarshal(r.Inputs, &t.Inputs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// currentConfig locks workspace's row (no run is queued meanwhile) and reads its current version and modules.
func currentConfig(ctx context.Context, q *db.Queries, workspaceID uuid.UUID) (uuid.NullUUID, []WorkspaceTemplate, error) {
	cv, err := q.CurrentConfig(ctx, workspaceID)
	if err != nil {
		return cv, nil, notFound(err)
	}
	mods, err := configModules(ctx, q, cv)
	return cv, mods, err
}

// newVersion stores mods as a new version of the workspace, made from parent (its current one).
func newVersion(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, parent uuid.NullUUID, mods []WorkspaceTemplate,
	source string, createdBy uuid.UUID) (uuid.UUID, error) {
	id, err := q.InsertConfigVersion(ctx, db.InsertConfigVersionParams{WorkspaceID: workspaceID, ParentID: parent, Source: source,
		CreatedBy: optID(createdBy)})
	if err != nil {
		return id, err
	}
	for _, m := range mods {
		raw, err := json.Marshal(m.Inputs)
		if err != nil {
			return id, err
		}
		if err := q.InsertConfigModule(ctx, db.InsertConfigModuleParams{CvID: id, Name: m.Name, TemplateID: m.TemplateID,
			ModuleSource: m.ModuleSource, ModuleRef: m.ModuleRef, Inputs: raw}); err != nil {
			return id, err
		}
	}
	return id, nil
}

// queueVersion stores mods as a new version made from the current one (cur) and queues an action run on it.
func queueVersion(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, cur uuid.NullUUID, mods []WorkspaceTemplate,
	source, action string, createdBy uuid.UUID) (Run, error) {
	cv, err := newVersion(ctx, q, workspaceID, cur, mods, source, createdBy)
	if err != nil {
		return Run{}, err
	}
	return queueRun(ctx, q, workspaceID, action, createdBy, optID(cv))
}

// setCurrent makes version cv the workspace's current one, without a run (a template removed with nothing
// deployed, the end of an applied import or removal).
func setCurrent(ctx context.Context, q *db.Queries, workspaceID, cv uuid.UUID) error {
	return q.SetCurrentConfig(ctx, db.SetCurrentConfigParams{ID: workspaceID, CvID: cv})
}

// withModule is mods with t in place of the module of its name, or added.
func withModule(mods []WorkspaceTemplate, t WorkspaceTemplate) []WorkspaceTemplate {
	out := slices.DeleteFunc(slices.Clone(mods), func(m WorkspaceTemplate) bool { return m.Name == t.Name })
	out = append(out, t)
	slices.SortFunc(out, func(a, b WorkspaceTemplate) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// without is mods without the modules named in names.
func without(mods []WorkspaceTemplate, names []string) []WorkspaceTemplate {
	return slices.DeleteFunc(slices.Clone(mods), func(m WorkspaceTemplate) bool { return slices.Contains(names, m.Name) })
}

// RunTemplates are the templates run runID plans (its version), by name; none for a run without one.
func (s *Store) RunTemplates(ctx context.Context, runID uuid.UUID) ([]WorkspaceTemplate, error) {
	rc, err := s.q.RunConfig(ctx, runID)
	if err != nil {
		return nil, notFound(err)
	}
	return configModules(ctx, s.q, rc.CvID)
}
