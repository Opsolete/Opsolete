-- Workspaces, their grants, VCS links and settings. Viewer filters use access_level (001_schema.sql).

-- name: InsertWorkspace :one
INSERT INTO workspaces (id, name, auto_apply, created_by, project_id)
SELECT COALESCE(sqlc.narg(id)::uuid, gen_random_uuid()), sqlc.arg(name)::text, sqlc.arg(auto_apply)::boolean, sqlc.narg(created_by)::uuid,
    -- an unknown project name is NULL: the insert fails instead of silently using the default
    CASE WHEN sqlc.arg(project)::text = '' THEN o.default_project_id ELSE (SELECT p.id FROM projects p WHERE p.name = sqlc.arg(project)::text) END
FROM organization o
RETURNING id;

-- name: WorkspaceIsRemote :one
SELECT execution_mode = 'remote' FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: GetWorkspaceByName :one
SELECT * FROM workspace_list WHERE name = sqlc.arg(name)::text;

-- name: GetWorkspaceByID :one
SELECT * FROM workspace_list WHERE id = sqlc.arg(id)::uuid;

-- name: ListWorkspaces :many
-- latest_*: the most recent run that isn't a drift check or speculative plan; NULL without one.
SELECT sqlc.embed(w), l.status AS latest_status, l.action AS latest_action, l.updated_at AS latest_updated_at
FROM workspace_list w
LEFT JOIN runs l ON l.id = (SELECT r.id FROM runs r
    WHERE r.workspace_id = w.id AND r.action NOT IN ('assess', 'plan') ORDER BY r.created_at DESC LIMIT 1)
WHERE (sqlc.arg(admin)::boolean OR access_level(w.id, sqlc.narg(viewer)::uuid) IS NOT NULL)
    AND (sqlc.arg(project)::text = '' OR w.project_name = sqlc.arg(project)::text)
ORDER BY COALESCE(l.updated_at, w.created_at) DESC LIMIT 200;

-- name: WorkspacesExist :one
SELECT NOT EXISTS (SELECT 1 FROM unnest(sqlc.arg(names)::text[]) n
    WHERE NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.name = n));

-- name: LockIdleWorkspace :one
-- Locks the workspace row (keeping new runs out until the transaction ends: their FK check waits).
-- active: a run is active; deployed: its current state manages resources or isn't indexed.
SELECT
    EXISTS (SELECT 1 FROM runs r WHERE r.workspace_id = w.id
        AND run_active(r.status))::boolean AS active,
    EXISTS (SELECT 1 FROM (SELECT v.id, v.indexed FROM state_versions v WHERE v.workspace_id = w.id ORDER BY v.id DESC LIMIT 1) cur
        WHERE NOT cur.indexed OR EXISTS (SELECT 1 FROM state_version_resources r WHERE r.state_version_id = cur.id))::boolean AS deployed
FROM workspaces w WHERE w.id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: WorkspaceStateLocked :one
SELECT (state_lock IS NOT NULL)::boolean FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: WorkspaceBlobKeys :many
SELECT blob_key FROM state_versions WHERE workspace_id = sqlc.arg(workspace_id)::uuid;

-- name: DeleteWorkspace :exec
DELETE FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: Access :one
SELECT COALESCE(access_level(sqlc.arg(workspace_id)::uuid, sqlc.narg(user_id)::uuid), '')::text;

-- name: WorkspaceGrants :many
SELECT group_name, level FROM workspace_access WHERE workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY group_name;

-- name: SetWorkspaceGrant :exec
INSERT INTO workspace_access (workspace_id, group_name, level) VALUES (sqlc.arg(workspace_id)::uuid, sqlc.arg(group_name)::text, sqlc.arg(level)::text)
ON CONFLICT (workspace_id, group_name) DO UPDATE SET level = EXCLUDED.level;

-- name: DeleteWorkspaceGrant :exec
DELETE FROM workspace_access WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND group_name = sqlc.arg(group_name)::text;

-- name: LockWorkspaceVCS :one
-- detach: the link or tag format changes, or pull request plans are turned on. FOR UPDATE: the
-- poller's run inserts (their foreign key check) wait until the change commits.
SELECT ((vcs.connection_id, vcs.repo, vcs.branch, COALESCE(vcs.tags_regex, '')) IS DISTINCT FROM
        (sqlc.narg(connection_id)::uuid, NULLIF(sqlc.arg(repo)::text, ''), NULLIF(sqlc.arg(branch)::text, ''), sqlc.arg(tags_regex)::text)
        OR (NOT COALESCE(vcs.pull_requests, false) AND sqlc.arg(pull_requests)::boolean))::boolean AS detach,
    EXISTS (SELECT 1 FROM runs r WHERE r.workspace_id = w.id
        AND run_active(r.status))::boolean AS active
FROM workspaces w LEFT JOIN workspace_vcs vcs ON vcs.workspace_id = w.id
WHERE w.id = sqlc.arg(workspace_id)::uuid FOR UPDATE OF w;

-- name: DeleteWorkspaceVCS :exec
DELETE FROM workspace_vcs WHERE workspace_id = sqlc.arg(workspace_id)::uuid;

-- name: UpsertWorkspaceVCS :exec
-- tag_baseline only changes with the connection, repository or tag format.
INSERT INTO workspace_vcs AS v (workspace_id, connection_id, repo, branch, trigger, pull_requests, tags_regex, tag_baseline)
VALUES (sqlc.arg(workspace_id)::uuid, sqlc.arg(connection_id)::uuid, sqlc.arg(repo)::text, sqlc.arg(branch)::text, sqlc.arg(trigger)::boolean,
    sqlc.arg(pull_requests)::boolean, sqlc.arg(tags_regex)::text, sqlc.arg(tag_baseline)::text)
ON CONFLICT (workspace_id) DO UPDATE SET connection_id = EXCLUDED.connection_id, repo = EXCLUDED.repo, branch = EXCLUDED.branch,
    trigger = EXCLUDED.trigger, pull_requests = EXCLUDED.pull_requests, tags_regex = EXCLUDED.tags_regex,
    tag_baseline = CASE WHEN (v.connection_id, v.repo, v.tags_regex) IS DISTINCT FROM (EXCLUDED.connection_id, EXCLUDED.repo, EXCLUDED.tags_regex)
        THEN EXCLUDED.tag_baseline ELSE v.tag_baseline END;

-- name: TouchWorkspace :exec
UPDATE workspaces SET updated_at = now() WHERE id = sqlc.arg(id)::uuid;

-- name: DetachWorkspaceSets :exec
DELETE FROM variable_set_workspaces WHERE workspace_id = sqlc.arg(workspace_id)::uuid;

-- name: VCSTriggered :many
SELECT * FROM workspace_list WHERE (vcs_trigger OR vcs_pull_requests) AND execution_mode = 'remote';

-- name: PinVCSCommit :one
UPDATE runs SET vcs_commit = COALESCE(vcs_commit, sqlc.arg(commit)::text) WHERE id = sqlc.arg(id)::uuid RETURNING vcs_commit::text;

-- name: UpdateGeneralSettings :execrows
UPDATE workspaces SET name = sqlc.arg(name)::text, execution_mode = sqlc.arg(execution_mode)::text,
    auto_apply = sqlc.arg(auto_apply)::boolean, updated_at = now()
WHERE id = sqlc.arg(id)::uuid;

-- name: SetDriftDetection :execrows
UPDATE workspaces SET drift_detection = sqlc.arg(drift_detection)::boolean, updated_at = now() WHERE id = sqlc.arg(id)::uuid;

-- name: SetGlobalRemoteState :execrows
UPDATE workspaces SET global_remote_state = sqlc.arg(global)::boolean, updated_at = now() WHERE id = sqlc.arg(id)::uuid;

-- name: ClearRemoteStateConsumers :exec
DELETE FROM remote_state_consumers WHERE workspace_id = sqlc.arg(workspace_id)::uuid;

-- name: AddRemoteStateConsumers :exec
-- unknown ids and the workspace itself are ignored
INSERT INTO remote_state_consumers (workspace_id, consumer_id)
SELECT sqlc.arg(workspace_id)::uuid, w.id FROM workspaces w
WHERE w.id = ANY(sqlc.arg(consumers)::uuid[]) AND w.id <> sqlc.arg(workspace_id)::uuid;

-- name: SetExecutionMode :exec
UPDATE workspaces SET execution_mode = sqlc.arg(mode)::text, updated_at = now() WHERE id = sqlc.arg(id)::uuid;

-- name: RemoteStateConsumers :many
SELECT w.id, w.name FROM remote_state_consumers c JOIN workspaces w ON w.id = c.consumer_id
WHERE c.workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY w.name;

-- name: SharesState :one
SELECT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = sqlc.arg(workspace_id)::uuid AND w.id <> sqlc.arg(consumer_id)::uuid
    AND (w.global_remote_state OR EXISTS (SELECT 1 FROM remote_state_consumers c
        WHERE c.workspace_id = w.id AND c.consumer_id = sqlc.arg(consumer_id)::uuid)));

-- name: ListRuns :many
-- assessments (drift checks) are not listed
SELECT * FROM run_list WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND action <> 'assess'
ORDER BY created_at DESC LIMIT sqlc.arg(lim)::integer;

-- name: RunMetrics :one
-- A phase lasts from its planning/applying entry to the next history entry.
WITH r AS (SELECT id, status FROM runs WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND action <> 'assess'),
h AS (SELECT h.status, lead(h.created_at) OVER (PARTITION BY h.run_id ORDER BY h.id) - h.created_at AS d
    FROM run_status_history h JOIN r ON r.id = h.run_id)
SELECT (SELECT count(*) FROM r)::bigint AS runs, (SELECT count(*) FROM r WHERE status = 'errored')::bigint AS failed,
    COALESCE(EXTRACT(EPOCH FROM (SELECT avg(d) FROM h WHERE status = 'planning')), 0)::float8 AS avg_plan,
    COALESCE(EXTRACT(EPOCH FROM (SELECT avg(d) FROM h WHERE status = 'applying')), 0)::float8 AS avg_apply;

-- name: LatestAssessment :one
SELECT * FROM run_list WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND action = 'assess' ORDER BY created_at DESC LIMIT 1;

-- name: CurrentOutputs :one
-- destroyed: the last successful run was a destroy (of the workspace, not of one template).
SELECT (SELECT v.outputs FROM state_versions v WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY v.id DESC LIMIT 1)::jsonb AS outputs,
    COALESCE((SELECT r.action = 'destroy' AND r.remove_template IS NULL FROM runs r
        WHERE r.workspace_id = sqlc.arg(workspace_id)::uuid AND r.status = 'applied' ORDER BY r.created_at DESC LIMIT 1), false)::boolean AS destroyed;

-- name: RunStatus :one
SELECT status FROM runs WHERE id = sqlc.arg(id)::uuid;

-- name: GetRun :one
SELECT * FROM run_list WHERE id = sqlc.arg(id)::uuid;

-- name: RunHistory :many
SELECT status, message, created_at FROM run_status_history WHERE run_id = sqlc.arg(run_id)::uuid ORDER BY id;

-- name: SetConfirmedBy :exec
UPDATE runs SET confirmed_by = COALESCE(sqlc.narg(confirmed_by)::uuid, confirmed_by) WHERE id = sqlc.arg(id)::uuid;
