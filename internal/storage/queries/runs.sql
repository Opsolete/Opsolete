-- Runs: queueing and the state machine's writes (storage/runs.go).
--
-- The Queue* queries share one shape: s selects at most one row per workspace, r inserts each in
-- plan_queued unless the workspace has an active run (runs_one_active_idx: the ON CONFLICT predicate
-- must match it exactly), h records the first history entry. They return the new runs.

-- name: QueueRun :one
-- A run of a remote workspace, queued by a user.
WITH s AS (
    SELECT w.id AS workspace_id, sqlc.arg(action)::text AS action, sqlc.narg(created_by)::uuid AS created_by,
        NULL::text AS vcs_commit, NULL::integer AS vcs_pull_request
    FROM workspaces w WHERE w.id = sqlc.arg(workspace_id)::uuid AND w.execution_mode = 'remote'
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request FROM s
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
        sqlc.arg(commit)::text AS vcs_commit, NULLIF(sqlc.arg(pull_request)::integer, 0) AS vcs_pull_request
    FROM workspaces w
    WHERE w.id = sqlc.arg(workspace_id)::uuid AND w.execution_mode = 'remote' AND NOT EXISTS (SELECT 1 FROM runs x
        WHERE x.workspace_id = w.id AND x.vcs_commit = sqlc.arg(commit)::text
            AND CASE WHEN sqlc.arg(pull_request)::integer > 0 THEN x.action = 'plan' AND x.vcs_pull_request = sqlc.arg(pull_request)::integer
                ELSE x.action NOT IN ('assess', 'plan') END)
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request FROM s
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
    SELECT w.id AS workspace_id, 'assess'::text AS action, NULL::uuid AS created_by, NULL::text AS vcs_commit, NULL::integer AS vcs_pull_request
    FROM workspaces w
    WHERE w.drift_detection AND w.execution_mode = 'remote'
        AND (EXISTS (SELECT 1 FROM workspace_vcs v WHERE v.workspace_id = w.id)
            OR EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id))
        AND EXISTS (SELECT 1 FROM state_version_resources sr WHERE sr.state_version_id =
            (SELECT max(v.id) FROM state_versions v WHERE v.workspace_id = w.id))
        AND NOT EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = w.id AND a.action = 'assess'
            AND a.created_at > now() - make_interval(secs => sqlc.arg(every_secs)::float8))
), r AS (
    INSERT INTO runs (workspace_id, action, status, created_by, vcs_commit, vcs_pull_request)
    SELECT s.workspace_id, s.action, 'plan_queued', s.created_by, s.vcs_commit, s.vcs_pull_request FROM s
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

-- name: SetRunPromotion :exec
UPDATE runs SET promotion = sqlc.arg(promotion)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunRollout :exec
UPDATE runs SET rollout_id = sqlc.arg(rollout_id)::uuid WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunAlwaysConfirm :exec
UPDATE runs SET always_confirm = true WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunScanResourceGroup :exec
UPDATE runs SET scan_resource_group = sqlc.arg(resource_group)::text WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunScanResult :exec
UPDATE runs SET candidates = sqlc.arg(candidates)::jsonb, inventory = sqlc.arg(inventory)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: RunScan :one
SELECT COALESCE(scan_resource_group, '')::text AS resource_group, candidates, inventory FROM runs WHERE id = sqlc.arg(id)::uuid AND action = 'scan';

-- name: RunImports :one
SELECT imports FROM runs WHERE id = sqlc.arg(id)::uuid;

-- name: SetRunImports :exec
-- An adoption: it always waits for a human, also with auto-apply.
UPDATE runs SET imports = sqlc.arg(imports)::jsonb, always_confirm = true WHERE id = sqlc.arg(id)::uuid;

-- name: LockRunStatus :one
SELECT status FROM runs WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: LockRun :one
SELECT status, workspace_id, remove_templates, config_changes, promotion FROM runs WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: UpdateRunStatus :exec
-- Only setStatus calls this. The lease ends unless the run starts working (a new claim: not acknowledged
-- yet); config_changes and promotion go when it ends.
UPDATE runs SET status = sqlc.arg(status)::text, updated_at = now(),
    lease_id = CASE WHEN sqlc.arg(leased)::boolean THEN lease_id END,
    lease_until = CASE WHEN sqlc.arg(leased)::boolean THEN lease_until END,
    lease_acked_at = NULL,
    config_changes = CASE WHEN sqlc.arg(ended)::boolean THEN NULL ELSE config_changes END,
    promotion = CASE WHEN sqlc.arg(ended)::boolean THEN NULL ELSE promotion END
WHERE id = sqlc.arg(id)::uuid;

-- name: AddRunHistory :exec
INSERT INTO run_status_history (run_id, status, message) VALUES (sqlc.arg(run_id)::uuid, sqlc.arg(status)::text, NULLIF(sqlc.arg(message)::text, ''));

-- name: ReleaseRunStateLock :execrows
-- only the lock the run's own Terraform took, never a rollback's or CLI's
UPDATE workspaces SET state_lock = NULL, state_locked_at = NULL
WHERE id = sqlc.arg(workspace_id)::uuid AND state_lock->>'OpsoleteRun' = sqlc.arg(run_id)::uuid::text;

-- name: DeleteWorkspaceTemplate :exec
DELETE FROM workspace_templates WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND name = sqlc.arg(name)::text;

-- name: ConfigSnapshot :one
SELECT template_config(t.template_id, t.module_source, t.module_ref, t.inputs)::jsonb FROM workspace_templates t
WHERE t.workspace_id = sqlc.arg(workspace_id)::uuid AND t.name = sqlc.arg(name)::text;

-- name: SetConfigChanges :exec
UPDATE runs SET config_changes = sqlc.arg(config_changes)::jsonb WHERE id = sqlc.arg(id)::uuid;

-- name: RestoreTemplate :exec
-- puts before back, only while the template is still at after
UPDATE workspace_templates t SET template_id = sqlc.arg(before)::jsonb->>'template_id', module_source = sqlc.arg(before)::jsonb->>'module_source',
    module_ref = sqlc.arg(before)::jsonb->>'module_ref', inputs = sqlc.arg(before)::jsonb->'inputs'
WHERE t.workspace_id = sqlc.arg(workspace_id)::uuid AND t.name = sqlc.arg(name)::text
    AND template_config(t.template_id, t.module_source, t.module_ref, t.inputs) = sqlc.arg(after)::jsonb;

-- name: RemoveRestoredTemplate :exec
-- removes a template a run added, only while it is still at after
DELETE FROM workspace_templates t
WHERE t.workspace_id = sqlc.arg(workspace_id)::uuid AND t.name = sqlc.arg(name)::text
    AND template_config(t.template_id, t.module_source, t.module_ref, t.inputs) = sqlc.arg(after)::jsonb;

-- name: RestoreRemovedTemplate :exec
-- puts back a template a run removed, unless a template took its name since
INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
VALUES (sqlc.arg(workspace_id)::uuid, sqlc.arg(name)::text, sqlc.arg(before)::jsonb->>'template_id', sqlc.arg(before)::jsonb->>'module_source',
    sqlc.arg(before)::jsonb->>'module_ref', sqlc.arg(before)::jsonb->'inputs')
ON CONFLICT (workspace_id, name) DO NOTHING;

-- name: ListWorkspaceTemplates :many
SELECT name, template_id, module_source, module_ref, inputs FROM workspace_templates
WHERE workspace_id = sqlc.arg(workspace_id)::uuid ORDER BY name;

-- name: AllTemplateInputs :many
SELECT workspace_id, name, inputs FROM workspace_templates;

-- name: ResealInputs :execrows
UPDATE workspace_templates SET inputs = sqlc.arg(inputs)::jsonb
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND name = sqlc.arg(name)::text AND inputs = sqlc.arg(old)::jsonb;

-- name: InsertWorkspaceTemplate :execrows
INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
VALUES (sqlc.arg(workspace_id)::uuid, sqlc.arg(name)::text, sqlc.arg(template_id)::text, sqlc.arg(module_source)::text,
    sqlc.arg(module_ref)::text, sqlc.arg(inputs)::jsonb);

-- name: UpdateWorkspaceTemplate :execrows
UPDATE workspace_templates SET template_id = sqlc.arg(template_id)::text, module_source = sqlc.arg(module_source)::text,
    module_ref = sqlc.arg(module_ref)::text, inputs = sqlc.arg(inputs)::jsonb
WHERE workspace_id = sqlc.arg(workspace_id)::uuid AND name = sqlc.arg(name)::text;

-- name: LockWorkspaceTemplate :one
-- FOR UPDATE: templates can't be added meanwhile (their FK check waits for the lock).
SELECT EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = w.id AND t.name = sqlc.arg(name)::text)::boolean
FROM workspaces w WHERE w.id = sqlc.arg(workspace_id)::uuid FOR UPDATE;

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
-- Drift checks, speculative plans and scans, history and logs with them (cascade), except what readers
-- need: a workspace's latest drift check (LatestAssessment) and latest one that found drift or none
-- (workspace_list.drifted), and a pull request's latest plan (QueueVCSRun's dedupe). Plans run by
-- hand (no pull request) have no exception.
DELETE FROM runs WHERE id IN (
    SELECT r.id FROM runs r WHERE r.action IN ('assess', 'plan', 'scan') AND NOT run_active(r.status)
        AND r.updated_at < now() - make_interval(secs => sqlc.arg(older_secs)::float8)
        AND CASE WHEN r.action = 'assess' THEN
                EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = r.workspace_id AND a.action = 'assess' AND a.created_at > r.created_at)
                AND (r.status NOT IN ('no_changes', 'drifted') OR EXISTS (SELECT 1 FROM runs a WHERE a.workspace_id = r.workspace_id
                    AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted') AND a.created_at > r.created_at))
            ELSE r.vcs_pull_request IS NULL OR EXISTS (SELECT 1 FROM runs p WHERE p.workspace_id = r.workspace_id
                AND p.action = 'plan' AND p.vcs_pull_request = r.vcs_pull_request AND p.created_at > r.created_at) END
    LIMIT sqlc.arg(lim)::integer FOR UPDATE OF r SKIP LOCKED
);

-- name: Notify :exec
-- Inside a transaction Postgres delivers it on commit, and never for one rolled back (notify in runs.go).
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);
