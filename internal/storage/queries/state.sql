-- State versions, their resource index and the state lock.
--
-- Lock ownership: a lock belongs to the run in state_lock->>'OpsoleteRun' (none for rollbacks, the
-- UI and the CLI). LockState, UnlockState and StateLockedBy compare it with their run_id, so a run
-- can't take over or release a lock of another run, a rollback, the UI or the CLI whose ID it saw in a 423.

-- name: WorkingRun :one
SELECT id, status, (vcs_pull_request IS NOT NULL)::boolean AS pull_request FROM runs
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND run_working(status) AND lease_acked_at IS NOT NULL;

-- name: StateVersions :many
SELECT v.id, v.serial, v.lineage, v.blob_key, v.md5, v.size, v.indexed,
    (SELECT count(*) FROM state_version_resources r WHERE r.state_version_id = v.id)::bigint AS resources,
    COALESCE((SELECT o.serial FROM state_versions o WHERE o.id = v.rollback_of), 0)::bigint AS copy_of, v.created_at
FROM state_versions v WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid AND v.id <= sqlc.arg(up_to)::bigint
ORDER BY v.id DESC LIMIT sqlc.arg(lim)::integer;

-- name: AddStateVersion :execrows
-- Only while its writer still may write: run_id (if set) applies with a lease, cloud_user (if set)
-- holds the cloud lock, rollback_lock (if set) is still the workspace's lock, taken by no run.
-- FOR SHARE keeps that true until commit: a force-cancel, force-unlock or stale-lock release waits.
INSERT INTO state_versions (workspace_id, serial, lineage, blob_key, md5, size, outputs, run_id)
SELECT sqlc.arg(workspace_id)::uuid, sqlc.arg(serial)::bigint, sqlc.arg(lineage)::text, sqlc.arg(blob_key)::text, sqlc.arg(md5)::text,
    sqlc.arg(size)::bigint, sqlc.narg(outputs)::jsonb, sqlc.narg(run_id)::uuid
WHERE (sqlc.narg(run_id)::uuid IS NULL OR EXISTS (SELECT 1 FROM runs
        WHERE id = sqlc.narg(run_id)::uuid AND status = 'applying' AND lease_id IS NOT NULL FOR SHARE))
    AND (sqlc.arg(cloud_user)::text = '' OR EXISTS (SELECT 1 FROM workspaces WHERE id = sqlc.arg(workspace_id)::uuid
        AND starts_with(state_lock->>'ID', sqlc.arg(cloud_prefix)::text) AND state_lock->>'UserID' = sqlc.arg(cloud_user)::text FOR SHARE))
    AND (sqlc.arg(rollback_lock)::text = '' OR EXISTS (SELECT 1 FROM workspaces WHERE id = sqlc.arg(workspace_id)::uuid
        AND state_lock->>'ID' = sqlc.arg(rollback_lock)::text AND state_lock->>'OpsoleteRun' IS NULL FOR SHARE));

-- name: DropIntermediateStates :many
DELETE FROM state_versions WHERE run_id = sqlc.arg(run_id)::uuid
    AND id < (SELECT max(x.id) FROM state_versions x WHERE x.run_id = sqlc.arg(run_id)::uuid)
RETURNING blob_key;

-- name: StateVersionBlob :one
SELECT blob_key FROM state_versions WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND id = sqlc.arg(id)::bigint;

-- name: CopyStateIndex :exec
-- the current version gets version from's index and becomes its copy: a rollback
WITH cur AS (SELECT v.id FROM state_versions v WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY v.id DESC LIMIT 1),
ins AS (INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module)
    SELECT cur.id, r.address, r.type, r.name, r.provider, r.module FROM cur, state_version_resources r
    WHERE r.state_version_id = sqlc.arg(from_id)::bigint ON CONFLICT DO NOTHING)
UPDATE state_versions SET indexed = (SELECT f.indexed FROM state_versions f WHERE f.id = sqlc.arg(from_id)::bigint), rollback_of = sqlc.arg(from_id)::bigint
WHERE state_versions.id = (SELECT cur.id FROM cur);

-- name: LockState :one
-- taken: the lock is now info's (free, or already lock_id's and the same run's); held: the lock now.
-- held can be '' when not taken: the statement's snapshot predates the winner's commit (CurrentStateLock).
WITH taken AS (
    UPDATE workspaces SET state_lock = (sqlc.arg(info)::jsonb - 'OpsoleteRun') || jsonb_strip_nulls(jsonb_build_object('OpsoleteRun', sqlc.narg(run_id)::uuid)),
        state_locked_at = CASE WHEN state_lock IS NULL THEN now() ELSE state_locked_at END
    WHERE id = sqlc.arg(id)::uuid AND (state_lock IS NULL OR state_lock->>'ID' = sqlc.arg(lock_id)::text
        AND state_lock->>'OpsoleteRun' IS NOT DISTINCT FROM sqlc.narg(run_id)::uuid::text)
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM taken)::boolean AS taken,
    COALESCE((SELECT w.state_lock::text FROM workspaces w WHERE w.id = sqlc.arg(id)::uuid), '')::text AS held;

-- name: CurrentStateLock :one
SELECT COALESCE((SELECT w.state_lock::text FROM workspaces w WHERE w.id = sqlc.arg(id)::uuid), '')::text;

-- name: ReleaseOrphanedLocks :many
-- a run's lock once that run no longer works with a lease; a rollback's after timeout_secs; never cloud locks
WITH stale AS (
    SELECT w.id, w.state_lock FROM workspaces w WHERE w.state_lock IS NOT NULL AND CASE
        WHEN w.state_lock->>'OpsoleteRun' IS NOT NULL THEN NOT EXISTS (SELECT 1 FROM runs r
            WHERE r.id::text = w.state_lock->>'OpsoleteRun' AND run_working(r.status) AND r.lease_id IS NOT NULL)
        ELSE NOT starts_with(w.state_lock->>'ID', sqlc.arg(cloud_prefix)::text)
            AND w.state_locked_at < now() - make_interval(secs => sqlc.arg(timeout_secs)::float8)
    END FOR UPDATE
)
UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL FROM stale WHERE workspaces.id = stale.id
RETURNING workspaces.id, stale.state_lock::text AS info;

-- name: UnlockState :execrows
UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL
WHERE id = sqlc.arg(id)::uuid AND state_lock->>'ID' = sqlc.arg(lock_id)::text
    AND state_lock->>'OpsoleteRun' IS NOT DISTINCT FROM sqlc.narg(run_id)::uuid::text;

-- name: StateLockedBy :one
SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = sqlc.arg(id)::uuid AND state_lock->>'ID' = sqlc.arg(lock_id)::text
    AND state_lock->>'OpsoleteRun' IS NOT DISTINCT FROM sqlc.narg(run_id)::uuid::text);

-- name: StateLockID :one
SELECT COALESCE(state_lock->>'ID', '')::text FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: GetStateLock :one
SELECT state_lock FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: UnlockCloudState :execrows
-- by user (or anyone: user ''); a run's Terraform picks its own lock ID: it can't make it a cloud one
UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL WHERE id = sqlc.arg(id)::uuid
    AND starts_with(state_lock->>'ID', sqlc.arg(cloud_prefix)::text) AND (sqlc.arg(user_id)::text = '' OR state_lock->>'UserID' = sqlc.arg(user_id)::text)
    AND state_lock->>'OpsoleteRun' IS NULL;

-- name: CloudLocked :one
SELECT COALESCE(starts_with(state_lock->>'ID', sqlc.arg(cloud_prefix)::text) AND state_lock->>'UserID' = sqlc.arg(user_id)::text, false)::boolean
FROM workspaces WHERE id = sqlc.arg(id)::uuid;

-- name: CurrentStateVersion :one
SELECT id FROM state_versions WHERE workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY id DESC LIMIT 1;

-- name: ClearStateIndex :exec
DELETE FROM state_version_resources WHERE state_version_id = sqlc.arg(id)::bigint;

-- name: InsertStateIndex :exec
-- ON CONFLICT: a duplicate address must not lose the whole index.
INSERT INTO state_version_resources (state_version_id, address, type, name, provider, module)
SELECT sqlc.arg(id)::bigint, r.address, r.type, r.name, r.provider, r.module FROM jsonb_to_recordset(sqlc.arg(resources)::jsonb)
    AS r(address text, type text, name text, provider text, module text)
ON CONFLICT DO NOTHING;

-- name: MarkIndexed :exec
UPDATE state_versions SET indexed = true WHERE id = sqlc.arg(id)::bigint;

-- name: UnknownResource :one
WITH cur AS (SELECT v.id FROM state_versions v WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY v.id DESC LIMIT 1)
SELECT a::text FROM unnest(sqlc.arg(addresses)::text[]) a
WHERE NOT EXISTS (SELECT 1 FROM cur JOIN state_version_resources r ON r.state_version_id = cur.id AND r.address = a)
LIMIT 1;

-- name: Resources :many
-- shortcut: no paging, capped at 2000 rows; add paging when inventories get that large.
WITH cur AS (SELECT v.id, v.workspace_id, v.serial FROM state_versions v
    WHERE v.workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY v.id DESC LIMIT 1)
SELECT r.address, r.type, r.name, r.provider, r.module, w.name AS workspace, cur.serial
FROM cur JOIN state_version_resources r ON r.state_version_id = cur.id JOIN workspaces w ON w.id = cur.workspace_id
ORDER BY r.type, r.address LIMIT 2000;
