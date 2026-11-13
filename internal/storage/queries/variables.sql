-- Variables and variable sets. Which sets reach a workspace: variable_set_rank (001_schema.sql).

-- name: ListVariables :many
-- exactly one of workspace_id and set_id is set
SELECT * FROM variables
WHERE workspace_id IS NOT DISTINCT FROM sqlc.narg(workspace_id)::uuid AND set_id IS NOT DISTINCT FROM sqlc.narg(set_id)::uuid
ORDER BY category, key;

-- name: AllVariables :many
SELECT * FROM variables;

-- name: ResealVariable :execrows
UPDATE variables SET value = sqlc.arg(sealed)::bytea WHERE id = sqlc.arg(id)::uuid AND value = sqlc.arg(old)::bytea;

-- name: SetWorkspaceVariable :exec
INSERT INTO variables (workspace_id, category, key, value, sensitive, hcl)
VALUES (sqlc.arg(workspace_id)::uuid, sqlc.arg(category)::text, sqlc.arg(key)::text, sqlc.arg(value)::bytea, sqlc.arg(sensitive)::boolean, sqlc.arg(hcl)::boolean)
ON CONFLICT (workspace_id, category, key) DO UPDATE SET value = EXCLUDED.value, sensitive = EXCLUDED.sensitive, hcl = EXCLUDED.hcl, updated_at = now();

-- name: SetSetVariable :exec
INSERT INTO variables (set_id, category, key, value, sensitive, hcl)
VALUES (sqlc.arg(set_id)::uuid, sqlc.arg(category)::text, sqlc.arg(key)::text, sqlc.arg(value)::bytea, sqlc.arg(sensitive)::boolean, sqlc.arg(hcl)::boolean)
ON CONFLICT (set_id, category, key) DO UPDATE SET value = EXCLUDED.value, sensitive = EXCLUDED.sensitive, hcl = EXCLUDED.hcl, updated_at = now();

-- name: DeleteVariable :exec
-- ids of other owners are ignored
DELETE FROM variables
WHERE workspace_id IS NOT DISTINCT FROM sqlc.narg(workspace_id)::uuid AND set_id IS NOT DISTINCT FROM sqlc.narg(set_id)::uuid
    AND id = sqlc.arg(id)::uuid;

-- name: RunVariables :many
-- one per category and key: the workspace's own, then by variable_set_rank, then the first set by name
SELECT DISTINCT ON (v.category, v.key) v.* FROM variables v LEFT JOIN variable_sets vs ON vs.id = v.set_id
WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid OR variable_set_rank(sqlc.arg(workspace_id)::uuid, v.set_id) IS NOT NULL
ORDER BY v.category, v.key, CASE WHEN v.workspace_id IS NOT NULL THEN 0 ELSE variable_set_rank(sqlc.arg(workspace_id)::uuid, v.set_id) END, vs.name;

-- name: CreateVariableSet :exec
INSERT INTO variable_sets (name, description, global) VALUES (sqlc.arg(name)::text, sqlc.arg(description)::text, sqlc.arg(global)::boolean);

-- name: VariableSets :many
SELECT * FROM variable_set_list ORDER BY name;

-- name: WorkspaceVariableSets :many
SELECT * FROM variable_set_list WHERE variable_set_rank(sqlc.arg(workspace_id)::uuid, id) IS NOT NULL ORDER BY name;

-- name: ProjectVariableSets :many
SELECT * FROM variable_set_list
WHERE id IN (SELECT a.set_id FROM variable_set_projects a WHERE a.project_id = sqlc.arg(project_id)::uuid) ORDER BY name;

-- name: GetVariableSet :one
SELECT * FROM variable_set_list WHERE name = sqlc.arg(name)::text;

-- name: UpdateVariableSet :exec
UPDATE variable_sets SET description = sqlc.arg(description)::text, global = sqlc.arg(global)::boolean WHERE id = sqlc.arg(id)::uuid;

-- name: DeleteVariableSet :exec
DELETE FROM variable_sets WHERE id = sqlc.arg(id)::uuid;

-- name: AttachSetWorkspace :exec
INSERT INTO variable_set_workspaces (set_id, workspace_id) SELECT sqlc.arg(set_id)::uuid, w.id FROM workspaces w WHERE w.name = sqlc.arg(name)::text
ON CONFLICT DO NOTHING;

-- name: DetachSetWorkspace :exec
DELETE FROM variable_set_workspaces
WHERE set_id = sqlc.arg(set_id)::uuid AND workspace_id = (SELECT w.id FROM workspaces w WHERE w.name = sqlc.arg(name)::text);

-- name: AttachSetProject :exec
INSERT INTO variable_set_projects (set_id, project_id) SELECT sqlc.arg(set_id)::uuid, p.id FROM projects p WHERE p.name = sqlc.arg(name)::text
ON CONFLICT DO NOTHING;

-- name: DetachSetProject :exec
DELETE FROM variable_set_projects
WHERE set_id = sqlc.arg(set_id)::uuid AND project_id = (SELECT p.id FROM projects p WHERE p.name = sqlc.arg(name)::text);

-- The Select* queries make a selection exactly names (NULL: none): the delete and the insert touch
-- disjoint rows, so one statement does both.

-- name: SelectSetWorkspaces :exec
WITH del AS (DELETE FROM variable_set_workspaces a WHERE a.set_id = sqlc.arg(id)::uuid
    AND a.workspace_id NOT IN (SELECT w.id FROM workspaces w WHERE w.name = ANY(sqlc.arg(names)::text[])))
INSERT INTO variable_set_workspaces (set_id, workspace_id)
SELECT sqlc.arg(id)::uuid, w.id FROM workspaces w WHERE w.name = ANY(sqlc.arg(names)::text[]) ON CONFLICT DO NOTHING;

-- name: SelectSetProjects :exec
WITH del AS (DELETE FROM variable_set_projects a WHERE a.set_id = sqlc.arg(id)::uuid
    AND a.project_id NOT IN (SELECT p.id FROM projects p WHERE p.name = ANY(sqlc.arg(names)::text[])))
INSERT INTO variable_set_projects (set_id, project_id)
SELECT sqlc.arg(id)::uuid, p.id FROM projects p WHERE p.name = ANY(sqlc.arg(names)::text[]) ON CONFLICT DO NOTHING;

-- name: SelectConnWorkspaces :exec
WITH del AS (DELETE FROM vcs_connection_workspaces a WHERE a.connection_id = sqlc.arg(id)::uuid
    AND a.workspace_id NOT IN (SELECT w.id FROM workspaces w WHERE w.name = ANY(sqlc.arg(names)::text[])))
INSERT INTO vcs_connection_workspaces (connection_id, workspace_id)
SELECT sqlc.arg(id)::uuid, w.id FROM workspaces w WHERE w.name = ANY(sqlc.arg(names)::text[]) ON CONFLICT DO NOTHING;

-- name: SelectConnProjects :exec
WITH del AS (DELETE FROM vcs_connection_projects a WHERE a.connection_id = sqlc.arg(id)::uuid
    AND a.project_id NOT IN (SELECT p.id FROM projects p WHERE p.name = ANY(sqlc.arg(names)::text[])))
INSERT INTO vcs_connection_projects (connection_id, project_id)
SELECT sqlc.arg(id)::uuid, p.id FROM projects p WHERE p.name = ANY(sqlc.arg(names)::text[]) ON CONFLICT DO NOTHING;
