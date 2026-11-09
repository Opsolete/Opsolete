-- Projects and their grants.

-- name: ListProjects :many
-- all for admins; else the default one, those with a workspace the viewer sees, and those granted to one of their groups
SELECT * FROM project_list p
WHERE sqlc.arg(admin)::boolean OR p.is_default
    OR EXISTS (SELECT 1 FROM workspaces w WHERE w.project_id = p.id AND access_level(w.id, sqlc.narg(viewer)::uuid) IS NOT NULL)
    OR EXISTS (SELECT 1 FROM project_access a JOIN users u ON u.id = sqlc.narg(viewer)::uuid
        WHERE a.project_id = p.id AND a.group_name = ANY(u.groups))
ORDER BY p.name;

-- name: CreatableProjects :many
-- all for admins; else those granted operate to one of the viewer's groups (the default one too: only then)
SELECT * FROM project_list p
WHERE sqlc.arg(admin)::boolean
    OR EXISTS (SELECT 1 FROM project_access a JOIN users u ON u.id = sqlc.narg(viewer)::uuid
        WHERE a.project_id = p.id AND a.level = 'operate' AND a.group_name = ANY(u.groups))
ORDER BY p.name;

-- name: GetProjectByName :one
SELECT * FROM project_list WHERE name = sqlc.arg(name)::text;

-- name: GetProjectByID :one
SELECT * FROM project_list WHERE id = sqlc.arg(id)::uuid;

-- name: DefaultProject :one
SELECT * FROM project_list WHERE is_default;

-- name: ProjectsExist :one
SELECT NOT EXISTS (SELECT 1 FROM unnest(sqlc.arg(names)::text[]) n
    WHERE NOT EXISTS (SELECT 1 FROM projects p WHERE p.name = n));

-- name: CreateProject :one
INSERT INTO projects (name, description) VALUES (sqlc.arg(name)::text, sqlc.arg(description)::text) RETURNING id;

-- name: UpdateProject :execrows
UPDATE projects SET name = sqlc.arg(name)::text, description = sqlc.arg(description)::text WHERE id = sqlc.arg(id)::uuid;

-- name: SetDefaultProject :execrows
UPDATE organization SET default_project_id = p.id FROM projects p WHERE p.id = sqlc.arg(id)::uuid;

-- name: DeleteProject :execrows
DELETE FROM projects WHERE id = sqlc.arg(id)::uuid;

-- name: MoveWorkspace :execrows
UPDATE workspaces w SET project_id = p.id, updated_at = now()
FROM projects p WHERE w.id = sqlc.arg(workspace_id)::uuid AND p.id = sqlc.arg(project_id)::uuid;

-- name: ProjectGrants :many
SELECT group_name, level FROM project_access WHERE project_id = sqlc.arg(project_id)::uuid ORDER BY group_name;

-- name: SetProjectGrant :exec
INSERT INTO project_access (project_id, group_name, level) VALUES (sqlc.arg(project_id)::uuid, sqlc.arg(group_name)::text, sqlc.arg(level)::text)
ON CONFLICT (project_id, group_name) DO UPDATE SET level = EXCLUDED.level;

-- name: DeleteProjectGrant :exec
DELETE FROM project_access WHERE project_id = sqlc.arg(project_id)::uuid AND group_name = sqlc.arg(group_name)::text;
