-- Jobs: runners claiming, heartbeating and finishing run phases. "Working" is status planning or
-- applying, the statuses a lease (lease_id) belongs to. A claim is used (heartbeat, log, finish,
-- artifacts, state) only once acknowledged (lease_acked_at): before that the claim may be undone.

-- name: ClaimableRun :one
-- The longest-waiting queued phase of a workspace whose project uses pool. SKIP LOCKED: runners never get the
-- same one. Runs of a locked workspace wait, except speculative plans and drift checks, which never take the state lock.
SELECT r.id, r.status FROM runs r JOIN workspaces w ON w.id = r.workspace_id JOIN projects p ON p.id = w.project_id
WHERE r.status IN ('plan_queued', 'apply_queued') AND p.runner_pool = sqlc.arg(pool)::text
    AND (r.action IN ('plan', 'assess') OR w.state_lock IS NULL)
ORDER BY r.updated_at FOR UPDATE OF r SKIP LOCKED LIMIT 1;

-- name: LeaseRun :one
UPDATE runs SET runner = sqlc.arg(runner)::text, lease_id = sqlc.arg(lease_id)::text,
    lease_until = now() + make_interval(secs => sqlc.arg(lease_secs)::float8)
WHERE id = sqlc.arg(id)::uuid
RETURNING workspace_id, action, replace, COALESCE(vcs_commit, '')::text AS vcs_commit, remove_templates, cv_id;

-- name: WorkspaceHasVCS :one
SELECT EXISTS (SELECT 1 FROM workspace_vcs WHERE workspace_id = sqlc.arg(workspace_id)::uuid);

-- name: AckLease :execrows
-- Idempotent: a retried acknowledgement keeps the first time.
UPDATE runs SET lease_acked_at = COALESCE(lease_acked_at, now()), lease_until = now() + make_interval(secs => sqlc.arg(ttl_secs)::float8)
WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND run_working(status);

-- name: Heartbeat :one
UPDATE runs SET lease_until = now() + make_interval(secs => sqlc.arg(ttl_secs)::float8)
WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND run_working(status) AND lease_acked_at IS NOT NULL
RETURNING cancel_requested;

-- name: HoldsLease :one
SELECT EXISTS (SELECT 1 FROM runs WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND status = sqlc.arg(status)::text
    AND lease_acked_at IS NOT NULL);

-- name: LeaseLogBytes :one
UPDATE runs SET lease_until = now() + make_interval(secs => sqlc.arg(ttl_secs)::float8), log_bytes = log_bytes + sqlc.arg(bytes)::bigint
WHERE id = sqlc.arg(id)::uuid AND lease_id = sqlc.arg(lease_id)::text AND run_working(status) AND lease_acked_at IS NOT NULL
RETURNING log_bytes, cancel_requested;

-- name: InsertLog :exec
INSERT INTO run_logs (run_id, chunk) VALUES (sqlc.arg(run_id)::uuid, sqlc.arg(chunk)::text);

-- name: RequestCancel :exec
UPDATE runs SET cancel_requested = true WHERE id = sqlc.arg(id)::uuid;

-- name: LockLeasedRun :one
-- The run, if lease_id holds it; the lease is checked here, setStatus then checks the transition.
-- needs_confirmation: a template rollout's run (see template_rollouts) or a promotion's always waits for a human.
SELECT r.status, r.action, r.workspace_id, w.auto_apply, (r.rollout_id IS NOT NULL OR r.always_confirm)::boolean AS needs_confirmation, r.cancel_requested
FROM runs r JOIN workspaces w ON w.id = r.workspace_id
WHERE r.id = sqlc.arg(id)::uuid AND r.lease_id = sqlc.arg(lease_id)::text AND run_working(r.status) AND r.lease_acked_at IS NOT NULL
FOR UPDATE OF r;

-- name: SetRunResult :exec
UPDATE runs SET plan = COALESCE(sqlc.narg(plan)::jsonb, plan), outputs = COALESCE(sqlc.narg(outputs)::jsonb, outputs)
WHERE id = sqlc.arg(id)::uuid;

-- name: ExpiredLeases :many
-- Acknowledged claims only (unacknowledged ones: UnackedLeases). SKIP LOCKED: a run being finished or canceled right now isn't waited for.
SELECT id FROM runs WHERE run_working(status) AND lease_acked_at IS NOT NULL AND lease_until < now()
FOR UPDATE SKIP LOCKED;

-- name: UnackedLeases :many
-- Claims not acknowledged within their lease (the ack window). SKIP LOCKED: an acknowledgement being written waits for nothing.
SELECT id, status, COALESCE(runner, '')::text AS runner FROM runs WHERE run_working(status) AND lease_id IS NOT NULL AND lease_acked_at IS NULL AND lease_until < now()
FOR UPDATE SKIP LOCKED;

-- name: ClearRunner :exec
UPDATE runs SET runner = NULL WHERE id = sqlc.arg(id)::uuid;

-- name: Logs :many
SELECT seq, chunk FROM run_logs WHERE run_id = sqlc.arg(run_id)::uuid AND seq > sqlc.arg(after)::bigint ORDER BY seq LIMIT sqlc.arg(lim)::integer;

-- name: Stats :one
SELECT (SELECT count(*) FROM workspaces)::bigint AS workspaces,
    COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(updated_at)) FROM runs WHERE status IN ('plan_queued', 'apply_queued')), 0)::float8 AS oldest_queued;

-- name: RunsByStatus :many
SELECT status, count(*)::bigint AS n FROM runs GROUP BY status;

-- name: AnyActiveRun :one
SELECT EXISTS (SELECT 1 FROM runs WHERE run_active(status));
