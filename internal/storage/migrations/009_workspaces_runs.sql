-- Workspaces and runs, HCP Terraform style. A workspace (formerly "deployment") is long-lived and
-- holds one state; every plan → (confirm) → apply, and every destroy, is a run. Runs are the job queue.
ALTER TABLE deployments RENAME TO workspaces;
ALTER TABLE workspaces ADD COLUMN name text, ADD COLUMN auto_apply boolean NOT NULL DEFAULT false;
UPDATE workspaces SET name = left(template_id, 40) || '-' || left(id::text, 8);
ALTER TABLE workspaces ALTER COLUMN name SET NOT NULL, ADD CONSTRAINT workspaces_name_key UNIQUE (name);

CREATE TABLE runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    action       text NOT NULL CHECK (action IN ('apply', 'destroy')),
    -- plan_queued → planning → needs_confirmation → apply_queued → applying → applied
    -- final: applied | no_changes | discarded | errored | canceled
    status       text NOT NULL,
    plan         jsonb,        -- what the plan will do: counts and addresses, never values
    outputs      jsonb,        -- non-sensitive outputs of a successful apply
    runner       text,
    lease_id     text,         -- fencing token of the runner working on the current phase
    lease_until  timestamptz,
    log_bytes    bigint NOT NULL DEFAULT 0,
    created_by   uuid REFERENCES users(id),
    confirmed_by uuid REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
-- One active run per workspace, so plans and applies of one state never interleave.
CREATE UNIQUE INDEX runs_one_active_idx ON runs (workspace_id)
    WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying');
CREATE INDEX runs_queue_idx ON runs (updated_at) WHERE status IN ('plan_queued', 'apply_queued');
CREATE INDEX runs_leases_idx ON runs (lease_until) WHERE lease_id IS NOT NULL;
CREATE INDEX runs_workspace_idx ON runs (workspace_id, created_at DESC);

-- Each deployment becomes its workspace's first run, with the same id, so logs and history carry over.
INSERT INTO runs (id, workspace_id, action, status, outputs, runner, log_bytes, created_by, created_at, updated_at)
SELECT id, id, action,
    CASE status WHEN 'succeeded' THEN 'applied' WHEN 'destroyed' THEN 'applied' WHEN 'canceled' THEN 'canceled'
        WHEN 'queued' THEN 'plan_queued' ELSE 'errored' END,
    outputs, runner, log_bytes, created_by, created_at, updated_at
FROM workspaces;

ALTER TABLE deployment_status_history RENAME TO run_status_history;
ALTER TABLE run_status_history RENAME COLUMN deployment_id TO run_id;
ALTER TABLE run_status_history DROP CONSTRAINT deployment_status_history_deployment_id_fkey,
    ADD CONSTRAINT run_status_history_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE;
-- A run in flight was planned and applied in one step by an older runner: it can't continue.
INSERT INTO run_status_history (run_id, status, message)
SELECT id, 'errored', 'interrupted by the upgrade to workspaces and runs' FROM workspaces WHERE status = 'running';

ALTER TABLE deployment_logs RENAME TO run_logs;
ALTER TABLE run_logs RENAME COLUMN deployment_id TO run_id;
ALTER TABLE run_logs DROP CONSTRAINT deployment_logs_deployment_id_fkey,
    ADD CONSTRAINT run_logs_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE;

ALTER TABLE state_versions RENAME COLUMN deployment_id TO workspace_id;

DROP INDEX IF EXISTS deployments_queue_idx;
DROP INDEX IF EXISTS deployments_leases_idx;
ALTER TABLE workspaces DROP COLUMN status, DROP COLUMN runner, DROP COLUMN lease_id, DROP COLUMN lease_until,
    DROP COLUMN log_bytes, DROP COLUMN outputs, DROP COLUMN action;
