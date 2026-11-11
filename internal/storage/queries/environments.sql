-- Per-environment input values of projects (002_environment_values.sql).

-- name: EnvironmentValues :many
SELECT e.template_id, e.input_name, e.value, e.keep_own, e.updated_at, COALESCE(u.email, '')::text AS updated_by
FROM environment_values e LEFT JOIN users u ON u.id = e.updated_by
WHERE e.project_id = sqlc.arg(project_id)::uuid
ORDER BY e.template_id, e.input_name;

-- name: WorkspaceEnvironmentValues :many
-- the values of the workspace's current project
SELECT e.template_id, e.input_name, e.value, e.keep_own
FROM environment_values e JOIN workspaces w ON w.project_id = e.project_id
WHERE w.id = sqlc.arg(workspace_id)::uuid;

-- name: SetEnvironmentValue :exec
INSERT INTO environment_values (project_id, template_id, input_name, value, keep_own, updated_by)
VALUES (sqlc.arg(project_id)::uuid, sqlc.arg(template_id)::text, sqlc.arg(input_name)::text, sqlc.arg(value)::text, sqlc.arg(keep_own)::boolean, sqlc.narg(updated_by)::uuid)
ON CONFLICT (project_id, template_id, input_name) DO UPDATE SET value = EXCLUDED.value, keep_own = EXCLUDED.keep_own, updated_at = now(), updated_by = EXCLUDED.updated_by;

-- name: ClearEnvironmentValue :exec
DELETE FROM environment_values
WHERE project_id = sqlc.arg(project_id)::uuid AND template_id = sqlc.arg(template_id)::text AND input_name = sqlc.arg(input_name)::text;
