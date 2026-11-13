-- Pending runs (as HCP's run queue): a run started while another is active waits as pending instead of being
-- refused; when the active run ends, the oldest pending one is queued (storage.promotePending). Pending isn't
-- active (run_active, runs_one_active_idx): it doesn't hold the workspace.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check CHECK (status IN ('pending', 'plan_queued', 'planning', 'needs_confirmation',
    'apply_queued', 'applying', 'applied', 'no_changes', 'drifted', 'planned', 'discarded', 'errored', 'canceled'));
CREATE INDEX runs_pending_idx ON runs (workspace_id, created_at) WHERE status = 'pending';
