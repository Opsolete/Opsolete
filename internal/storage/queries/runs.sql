-- Runs: queueing and the state machine's writes (storage/runs.go).
--
-- The Queue* queries share one shape: s selects at most one row per workspace, r inserts each in
-- plan_queued unless the workspace has an active run (runs_one_active_idx: the ON CONFLICT predicate
-- must match it exactly), h records the first history entry. They return the new runs.

-- name: QueueRun :one
-- A run of a remote workspace, queued by a user: of configuration version cv_id, else the workspace's current one.
WITH s AS (
    SELECT w.id AS workspace_id, sqlc.arg(action)::text AS action, sqlc.narg(created_by)::uuid AS created_by,
        NULL::text AS vcs_commit, NULL::integer AS vcs_pull_request, COALESCE(sqlc.narg(cv_id)::uuid, w.current_cv_id) AS cv_id
    FROM workspaces w WHERE w.id = sqlc.arg(workspace_id)::uuid AND w.execution_mode = 'remote'
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request, cv_id)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request, s.cv_id FROM s
    ON CONFLICT (workspace_id) WHERE run_active(status) DO NOTHING
    RETURNING runs.id, runs.workspace_id, runs.action, runs.created_at
), h AS (
    INSERT INTO run_status_history (run_id, status, message) SELECT r.id, 'plan_queued', sqlc.arg(message)::text FROM r
)
SELECT r.id, r.workspace_id, r.action, r.created_at FROM r;

-- name: QueueVCSRun :one
-- A run of a commit, pinned at once: an apply run for a push (pull_request 0), else a speculative
-- plan. None if a run of this commit exists: for a push any run but a drift check or speculative
-- plan, for a pull request a speculative plan of it.
WITH s AS (
    SELECT w.id AS workspace_id, sqlc.arg(action)::text AS action, NULL::uuid AS created_by,
        sqlc.arg(commit)::text AS vcs_commit, NULLIF(sqlc.arg(pull_request)::integer, 0) AS vcs_pull_request, w.current_cv_id AS cv_id
    FROM workspaces w
    WHERE w.id = sqlc.arg(workspace_id)::uuid AND w.execution_mode = 'remote' AND NOT EXISTS (SELECT 1 FROM runs x
        WHERE x.workspace_id = w.id AND x.vcs_commit = sqlc.arg(commit)::text
            AND CASE WHEN sqlc.arg(pull_request)::integer > 0 THEN x.action = 'plan' AND x.vcs_pull_request = sqlc.arg(pull_request)::integer
                ELSE x.action NOT IN ('assess', 'plan') END)
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request, cv_id)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request, s.cv_id FROM s
    ON CONFLICT (workspace_id) WHERE run_active(status) DO NOTHING
    RETURNING runs.id, runs.workspace_id, runs.action, runs.created_at
), h AS (
    INSERT INTO run_status_history (run_id, status, message) SELECT r.id, 'plan_queued', sqlc.arg(message)::text FROM r
)
SELECT r.id, r.workspace_id, r.action, r.created_at FROM r;

-- name: QueueAssessments :many
-- An assess run on every remote workspace with drift detection and configuration whose current
-- state manages resources and that wasn't assessed within every_secs.
WITH s AS (
    SELECT w.id AS workspace_id, 'assess'::text AS action, NULL::uuid AS created_by, NULL::text AS vcs_commit, NULL::integer AS vcs_pull_request,
        w.current_cv_id AS cv_id
    FROM workspaces w
    WHERE w.drift_detection AND w.execution_mode = 'remote'
        AND (EXISTS (SELECT 1 FROM workspace_vcs v WHERE v.workspace_id = w.id)
            OR EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id))
        AND EXISTS (SELECT 1 FROM state_version_resources sr WHERE sr.state_version_id =
            (SELECT max(v.id) FROM state_versions v WHERE v.workspace_id = w.id))
        AND NOT EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = w.id AND a.action = 'assess'
            AND a.created_at > now() - make_interval(secs => sqlc.arg(every_secs)::float8))
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request, cv_id)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request, s.cv_id FROM s
    ON CONFLICT (workspace_id) WHERE run_active(status) DO NOTHING
    RETURNING runs.id, runs.workspace_id, runs.action, runs.created_at
), h AS (
    INSERT INTO run_status_history (run_id, status, message)
    SELECT r.id, 'plan_queued', 'drift detection: refresh-only plan queued, waiting for a runner' FROM r
)
SELECT r.id, r.workspace_id, r.action, r.created_at FROM r;

-- name: SetRunReplace :exec
UPDATE runs SET replace = sqlc.arg(replace)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunRemoveTemplates :exec
UPDATE runs SET remove_templates = sqlc.arg(remove_templates)::text[] WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunRollout :exec
UPDATE runs SET rollout_id = sqlc.arg(rollout_id)::uuid WHERE id = sqlc.arg(id)::uuid;

-- name: SetConfirmReason :exec
UPDATE runs SET confirm_reason = sqlc.arg(reason)::text WHERE id = sqlc.arg(id)::uuid;

-- name: RunPlan :one
SELECT plan FROM runs WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunFound :exec
UPDATE runs SET found = sqlc.arg(found)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: RunFound :one
SELECT found FROM runs WHERE id = sqlc.arg(id)::uuid;

-- name: LockRunStatus :one
SELECT status, cancel_requested FROM runs WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: LockRun :one
-- promotes: a promotion's destroy run whose apply run isn't queued yet (storage.continuePromotion).
SELECT r.status, r.action, r.workspace_id, r.remove_templates, r.cv_id,
    EXISTS (SELECT 1 FROM promotions p WHERE p.destroy_run_id = r.id AND p.apply_run_id IS NULL)::boolean AS promotes
FROM runs r WHERE r.id = sqlc.arg(id)::uuid FOR UPDATE OF r;

-- name: UpdateRunStatus :exec
-- Only setStatus calls this. The lease ends unless the run starts working (a new claim: not acknowledged
-- yet).
UPDATE runs SET status = sqlc.arg(status)::text, updated_at = now(),
    lease_id = CASE WHEN sqlc.arg(leased)::boolean THEN lease_id END,
    lease_until = CASE WHEN sqlc.arg(leased)::boolean THEN lease_until END,
    lease_acked_at = NULL
WHERE id = sqlc.arg(id)::uuid;

-- name: AddRunHistory :exec
INSERT INTO run_status_history (run_id, status, message) VALUES (sqlc.arg(run_id)::uuid, sqlc.arg(status)::text, NULLIF(sqlc.arg(message)::text, ''));

-- name: ReleaseRunStateLock :execrows
-- only the lock the run's own Terraform took, never a rollback's or CLI's
UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL
WHERE id = sqlc.arg(workspace_id)::uuid AND state_lock->>'OpsoleteRun' = sqlc.arg(run_id)::uuid::text;

-- name: ListWorkspaceTemplates :many
SELECT name, template_id, module_source, module_ref, inputs, follows, importing FROM workspace_templates
WHERE workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY name;

-- Retention (storage.PruneRunLogs, storage.PruneSpeculativeRuns): at most lim runs per statement, so
-- a first pass over a big database holds short locks; ended runs only, updated_at is when they ended.

-- name: PruneRunLogs :many
-- Marks every ended run it passes (so it isn't picked again); had_log: it had a log to remove.
WITH r AS (
    SELECT id FROM runs WHERE log_removed_at IS NULL AND NOT run_active(status)
        AND updated_at < now() - make_interval(secs => sqlc.arg(older_secs)::float8)
    LIMIT sqlc.arg(lim)::integer FOR UPDATE SKIP LOCKED
), d AS (
    DELETE FROM run_logs l USING r WHERE l.run_id = r.id
)
UPDATE runs SET log_removed_at = now() FROM r WHERE runs.id = r.id
RETURNING (runs.log_bytes > 0)::boolean AS had_log;

-- name: PruneSpeculativeRuns :execrows
-- Drift checks and speculative plans, history and logs with them (cascade), except what readers
-- need: a workspace's latest drift check (LatestAssessment) and latest one that found drift or none
-- (workspace_list.drifted), a pull request's latest plan (QueueVCSRun's dedupe), and an import's plan
-- rounds still pending (storage.RunPending: their configuration imports, isn't applied, and is the current one
-- or made from it): the import session lives on them. Other plans run by hand have no exception.
DELETE FROM runs WHERE id IN (
    SELECT r.id FROM runs r WHERE r.action IN ('assess', 'plan') AND NOT run_active(r.status)
        AND r.updated_at < now() - make_interval(secs => sqlc.arg(older_secs)::float8)
        AND CASE WHEN r.action = 'assess' THEN
                EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = r.workspace_id AND a.action = 'assess' AND a.created_at > r.created_at)
                AND (r.status NOT IN ('no_changes', 'drifted') OR EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = r.workspace_id
                    AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted') AND a.created_at > r.created_at))
            ELSE (r.vcs_pull_request IS NULL OR EXISTS (SELECT 1 FROM runs p WHERE p.workspace_id = r.workspace_id
                AND p.action = 'plan' AND p.vcs_pull_request = r.vcs_pull_request AND p.created_at > r.created_at))
                AND NOT EXISTS (SELECT 1 FROM configuration_modules m JOIN configuration_versions cv ON cv.id = m.cv_id
                    JOIN workspaces w ON w.id = cv.workspace_id
                    WHERE m.cv_id = r.cv_id AND m.importing AND (cv.id = w.current_cv_id OR cv.parent_id IS NOT DISTINCT FROM w.current_cv_id)
                    AND NOT EXISTS (SELECT 1 FROM runs x JOIN run_status_history h ON h.run_id = x.id WHERE x.cv_id = cv.id
                        AND x.action NOT IN ('plan', 'assess') AND h.status IN ('applying', 'no_changes'))) END
    LIMIT sqlc.arg(lim)::integer FOR UPDATE OF r SKIP LOCKED
);

-- name: Notify :exec
-- Inside a transaction Postgres delivers it on commit, and never for one rolled back (notify in runs.go).
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);

-- name: SetRunDiff :exec
UPDATE runs SET plan_diff = sqlc.arg(plan_diff)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: RunDiff :one
SELECT plan_diff FROM runs WHERE id = sqlc.arg(id)::uuid;
