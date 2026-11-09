package storage

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage/db"
)

// EnvironmentValue is a project's value for a per-environment template input, in the input's
// canonical form string. UpdatedAt and UpdatedBy (an email, "" if unknown) only from EnvironmentValues.
type EnvironmentValue struct {
	TemplateID, Input, Value string
	UpdatedAt                time.Time
	UpdatedBy                string
}

// EnvironmentValues lists a project's values by template id and input name.
func (s *Store) EnvironmentValues(ctx context.Context, projectID uuid.UUID) ([]EnvironmentValue, error) {
	rows, err := s.q.EnvironmentValues(ctx, projectID)
	out := make([]EnvironmentValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, EnvironmentValue{TemplateID: r.TemplateID, Input: r.InputName, Value: r.Value, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy})
	}
	return out, err
}

// WorkspaceEnvironmentValues lists the values of the workspace's current project.
func (s *Store) WorkspaceEnvironmentValues(ctx context.Context, workspaceID uuid.UUID) ([]EnvironmentValue, error) {
	rows, err := s.q.WorkspaceEnvironmentValues(ctx, workspaceID)
	out := make([]EnvironmentValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, EnvironmentValue{TemplateID: r.TemplateID, Input: r.InputName, Value: r.Value})
	}
	return out, err
}

// SetEnvironmentValue sets (or replaces) a project's value for a template's input.
func (s *Store) SetEnvironmentValue(ctx context.Context, projectID uuid.UUID, templateID, input, value string, by uuid.UUID) error {
	return s.q.SetEnvironmentValue(ctx, db.SetEnvironmentValueParams{ProjectID: projectID, TemplateID: templateID, InputName: input,
		Value: value, UpdatedBy: optID(by)})
}

// ClearEnvironmentValue removes a project's value for a template's input; none is not an error.
func (s *Store) ClearEnvironmentValue(ctx context.Context, projectID uuid.UUID, templateID, input string) error {
	return s.q.ClearEnvironmentValue(ctx, db.ClearEnvironmentValueParams{ProjectID: projectID, TemplateID: templateID, InputName: input})
}
