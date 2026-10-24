ALTER TABLE deployments
    ADD COLUMN pipeline_id bigint,   -- pipeline the run belongs to, so later config changes don't orphan it
    ADD COLUMN run_url     text;

CREATE INDEX deployments_active_idx ON deployments (status) WHERE status IN ('queued', 'running');
