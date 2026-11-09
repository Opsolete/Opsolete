-- Per-environment template inputs (template field per_environment): a project's value for an input
-- of a template, applied to every workspace in the project at each run, over the workspace's own.
-- value is the input's canonical form string ("7", "true"), as templates hold defaults.
CREATE TABLE environment_values (
    project_id  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    template_id text NOT NULL,
    input_name  text NOT NULL,
    value       text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid REFERENCES users ON DELETE SET NULL,
    PRIMARY KEY (project_id, template_id, input_name)
);
