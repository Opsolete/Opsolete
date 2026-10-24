-- Native runners: deployments double as the job queue (status 'queued', no pipeline run).
ALTER TABLE deployments
    ADD COLUMN runner      text,         -- name of the runner that claimed the job
    ADD COLUMN lease_id    text,         -- fencing token: only the current lease holder may write
    ADD COLUMN lease_until timestamptz,  -- runner must heartbeat before this, or the job is failed
    ADD COLUMN log_bytes   bigint NOT NULL DEFAULT 0,
    ADD COLUMN module_source text;       -- what was deployed, independent of later template edits

CREATE INDEX deployments_queue_idx ON deployments (created_at) WHERE status = 'queued' AND pipeline_run_id IS NULL;
CREATE INDEX deployments_leases_idx ON deployments (lease_until) WHERE status = 'running' AND lease_id IS NOT NULL;

CREATE TABLE deployment_logs (
    seq           bigserial PRIMARY KEY,   -- global order; also the SSE event id / resume cursor
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    chunk         text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deployment_logs_idx ON deployment_logs (deployment_id, seq);

-- Registry tokens (Terraform clients) and runner tokens are separate: neither can act as the other.
ALTER TABLE api_tokens
    ADD COLUMN scope text NOT NULL DEFAULT 'registry' CHECK (scope IN ('registry', 'runner'));
