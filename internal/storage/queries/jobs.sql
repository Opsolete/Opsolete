-- Jobs: runners claiming, heartbeating and finishing run phases. "Working" is status planning or
-- applying, the statuses a lease (lease_id) belongs to.

-- name: ClaimableRun :one
-- The longest-waiting queued phase. SKIP LOCKED: runners never get the same one. Runs of a locked
-- workspace wait, except speculative plans and drift checks, which never take the state lock.
SELECT r.id, r.status FROM runs r WHERE r.status IN ('plan_queued', 'apply_queued')
    AND (r.action IN ('plan', 'assess') OR NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = r.workspace_id AND w.state_lock IS NOT NULL))
ORDER BY r.updated_at FOR UPDATE OF r SKIP LOCKED LIMIT 1;

-- name: LeaseRun :one
UPDATE runs SET runner = sqlc.arg(runner)::text, lease_id = sqlc.arg(lease_id)::text,
    lease_until = now() + make_interval(secs => sqlc.arg(lease_secs)::float8)
WHERE id = sqlc.arg(id)::uuid
RETURNING workspace_id, action, replace, COALESCE(vcs_commit, '')::text AS vcs_commit, COALESCE(remove_template, '')::text AS remove_template;

-- name: WorkspaceHasVCS :one
SELECT EXISTS (SELECT 1 FROM workspace_vcs WHERE workspace_id = sqlc.arg(workspace_id)::uuid);

-- name: Heartbeat :one
UPDATE runs SET lease_until = now() + make_interval(secs => sqlc.arg(ttl_secs)::float8)
WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND run_working(status)
RETURNING cancel_requested;

-- name: HoldsLease :one
SELECT EXISTS (SELECT 1 FROM runs WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND status = sqlc.arg(status)::text);

-- name: LeaseLogBytes :one
UPDATE runs SET lease_until = now() + make_interval(secs => sqlc.arg(ttl_secs)::float8), log_bytes = log_bytes + sqlc.arg(bytes)::bigint
WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND run_working(status)
RETURNING log_bytes, cancel_requested;

-- name: InsertLog :exec
INSERT INTO run_logs (run_id, chunk) VALUES (sqlc.arg(run_id)::uuid, sqlc.arg(chunk)::text);

-- name: RequestCancel :exec
UPDATE runs SET cancel_requested = true WHERE id = sqlc.arg(id)::uuid;

-- name: LockLeasedRun :one
-- The run, if lease_id holds it; the lease is checked here, setStatus then checks the transition.
-- needs_confirmation: a template rollout's run (see template_rollouts) always waits for a human.
SELECT r.status, r.action, r.workspace_id, w.auto_apply, (r.rollout_id IS NOT NULL)::boolean AS needs_confirmation, r.cancel_requested
FROM runs r JOIN workspaces w ON w.id = r.workspace_id
WHERE r.id = sqlc.arg(id)::uuid AND r.lease_id = sqlc.arg(lease_id)::text AND run_working(r.status)
FOR UPDATE OF r;

-- name: SetRunResult :exec
UPDATE runs SET plan = COALESCE(sqlc.narg(plan)::jsonb, plan), outputs = COALESCE(sqlc.narg(outputs)::jsonb, outputs)
WHERE id = sqlc.arg(id)::uuid;

-- name: ExpiredLeases :many
-- SKIP LOCKED: a run being finished or canceled right now isn't waited for.
SELECT id FROM runs WHERE run_working(status) AND lease_id IS NOT NULL AND lease_until < now()
FOR UPDATE SKIP LOCKED;

-- name: Logs :many
SELECT seq, chunk FROM run_logs WHERE run_id = sqlc.arg(run_id)::uuid AND seq > sqlc.arg(after)::bigint ORDER BY seq LIMIT sqlc.arg(lim)::integer;

-- name: Stats :one
SELECT (SELECT count(*) FROM workspaces)::bigint AS workspaces,
    COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(updated_at)) FROM runs WHERE status IN ('plan_queued', 'apply_queued')), 0)::float8 AS oldest_queued;

-- name: RunsByStatus :many
SELECT status, count(*)::bigint AS n FROM runs GROUP BY status;

-- name: AnyActiveRun :one
SELECT EXISTS (SELECT 1 FROM runs WHERE run_active(status));
