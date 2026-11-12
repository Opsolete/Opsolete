-- Template uses and rollouts.

-- name: TemplateUses :many
-- every use of a template, by workspace name and module, with the workspace's active run (if any)
SELECT w.id AS workspace_id, w.name AS workspace, p.name AS project_name, w.execution_mode,
    u.name AS module, u.module_source, u.module_ref, u.inputs, u.follows, r.id AS run_id, r.status AS run_status
FROM workspace_templates u
JOIN workspaces w ON w.id = u.workspace_id
JOIN projects p ON p.id = w.project_id
LEFT JOIN runs r ON r.workspace_id = w.id AND run_active(r.status)
WHERE u.template_id = sqlc.arg(template_id)::text AND (sqlc.narg(project_id)::uuid IS NULL OR w.project_id = sqlc.narg(project_id)::uuid)
ORDER BY w.name, u.name;

-- name: RolloutStatusForShare :one
-- FOR SHARE: a pause or stop waits until the caller's run is queued, and is seen by the next
SELECT status FROM template_rollouts WHERE id = sqlc.arg(id)::uuid FOR SHARE;

-- name: CreateRollout :one
-- ticked_at now: its first turn is the caller's
INSERT INTO template_rollouts (template_id, module_source, module_ref, project_id, batch_size, created_by, ticked_at)
VALUES (sqlc.arg(template_id)::text, sqlc.arg(module_source)::text, sqlc.arg(module_ref)::text, sqlc.narg(project_id)::uuid,
    sqlc.arg(batch_size)::integer, sqlc.narg(created_by)::uuid, now())
RETURNING id;

-- name: GetRollout :one
SELECT * FROM rollout_list WHERE id = sqlc.arg(id)::uuid;

-- name: LatestRollout :one
SELECT * FROM rollout_list WHERE template_id = sqlc.arg(template_id)::text ORDER BY created_at DESC LIMIT 1;

-- name: DueRollouts :many
-- claims the active rollouts no app instance worked on within every_secs, so instances take turns
WITH due AS (
    UPDATE template_rollouts SET ticked_at = now() WHERE status = 'active'
        AND (ticked_at IS NULL OR ticked_at < now() - make_interval(secs => sqlc.arg(every_secs)::float8)) RETURNING id)
SELECT o.* FROM rollout_list o JOIN due ON due.id = o.id ORDER BY o.created_at;

-- name: SetRolloutStatus :execrows
-- resuming (active) forgets the run failures before, and its next turn is the caller's
UPDATE template_rollouts SET status = sqlc.arg(status)::text, message = sqlc.arg(message)::text,
    resumed_at = CASE WHEN sqlc.arg(status)::text = 'active' THEN now() ELSE resumed_at END,
    ticked_at = CASE WHEN sqlc.arg(status)::text = 'active' THEN now() ELSE ticked_at END
WHERE id = sqlc.arg(id)::uuid AND status IN ('active', 'paused');

-- name: RolloutRuns :one
SELECT
    (SELECT count(*) FROM runs WHERE rollout_id = sqlc.arg(id)::uuid
        AND run_active(status))::bigint AS open,
    COALESCE((SELECT 'the run in ' || w.name || ': ' || replace(r.status, '_', ' ') FROM runs r
        JOIN workspaces w ON w.id = r.workspace_id
        WHERE r.rollout_id = sqlc.arg(id)::uuid AND r.status IN ('errored', 'discarded', 'canceled') AND r.updated_at > sqlc.arg(since)::timestamptz
        ORDER BY r.updated_at LIMIT 1), '')::text AS failed;
