-- Azure Pipelines executor removed: Form2Stack runners are the only execution path.
-- Fail what runners can't execute first, so it is never claimed: in-flight pipeline runs
-- and deployments recorded before module sources were stored.
WITH f AS (
    UPDATE deployments SET status = 'failed', updated_at = now()
    WHERE status IN ('pending', 'queued', 'running') AND lease_id IS NULL
      AND (pipeline_run_id IS NOT NULL OR module_source IS NULL)
    RETURNING id
)
INSERT INTO deployment_status_history (deployment_id, status, message)
SELECT id, 'failed', 'not executable after the Azure Pipelines executor was removed; deploy again' FROM f;

DROP INDEX IF EXISTS deployments_active_idx;
DROP INDEX IF EXISTS deployments_queue_idx;
ALTER TABLE deployments DROP COLUMN pipeline_id, DROP COLUMN pipeline_run_id, DROP COLUMN run_url;
CREATE INDEX deployments_queue_idx ON deployments (created_at) WHERE status = 'queued';
