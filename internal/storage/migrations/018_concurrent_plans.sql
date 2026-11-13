-- Drift checks and pull requests' speculative plans run beside the workspace's other runs: they read the state
-- without locking it, write nothing and run the current configuration, so they don't hold the workspace.
-- run_blocking is an active run that does (one per workspace); the others are unique per kind instead: one
-- active drift check per workspace, one active plan per pull request.
-- Self-contained (run_active's statuses written out): PostgreSQL inlines index predicates with a restricted
-- search_path, where run_active can't be found by name.
CREATE FUNCTION run_blocking(status text, action text, pull_request integer) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
        AND action <> 'assess' AND pull_request IS NULL
$$;

DROP INDEX runs_one_active_idx;
CREATE UNIQUE INDEX runs_one_active_idx ON runs (workspace_id) WHERE run_blocking(status, action, vcs_pull_request);
CREATE UNIQUE INDEX runs_one_assess_idx ON runs (workspace_id)
    WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying') AND action = 'assess';
CREATE UNIQUE INDEX runs_one_pull_request_idx ON runs (workspace_id, vcs_pull_request)
    WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying') AND vcs_pull_request IS NOT NULL;

-- the templates a workspace runs: the blocking run's version while there is one (drift checks and pull request
-- plans run the current one anyway)
CREATE OR REPLACE VIEW workspace_templates AS
SELECT w.id AS workspace_id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs, m.follows, m.importing
FROM workspaces w JOIN configuration_modules m ON m.cv_id = COALESCE(
    (SELECT r.cv_id FROM runs r WHERE r.workspace_id = w.id AND run_blocking(r.status, r.action, r.vcs_pull_request)), w.current_cv_id);
