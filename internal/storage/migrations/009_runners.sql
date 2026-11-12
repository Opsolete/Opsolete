-- runners: one row per runner, with its own token (only the SHA-256 is stored), replacing the shared RUNNER_TOKEN.
-- Deleting a row revokes the runner. pool: the runners that may get a project's jobs (projects.runner_pool).
CREATE TABLE runners (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE,
    pool         text NOT NULL DEFAULT 'default',
    token_hash   bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);

ALTER TABLE projects ADD COLUMN runner_pool text NOT NULL DEFAULT 'default';

CREATE OR REPLACE VIEW project_list AS
SELECT p.id, p.name, p.description, (p.id = o.default_project_id)::boolean AS is_default,
    (SELECT count(*) FROM workspaces w WHERE w.project_id = p.id)::bigint AS workspaces,
    (SELECT count(*) FROM project_access a WHERE a.project_id = p.id)::bigint AS grants, p.created_at, p.runner_pool
FROM projects p, organization o;
