-- Indexes for queries that scanned runs or other tables on hot paths: the rollout tick (every 30s,
-- RolloutRuns and TemplateUses), the drift flag in every workspace read (workspaceCols, also
-- LatestAssessment and QueueAssessments), and VCS connection usage counts and their ON DELETE.
CREATE INDEX runs_rollout_idx ON runs (rollout_id) WHERE rollout_id IS NOT NULL;
CREATE INDEX runs_assess_idx ON runs (workspace_id, created_at DESC) WHERE action = 'assess';
CREATE INDEX runs_applied_idx ON runs (workspace_id, updated_at) WHERE status = 'applied';
CREATE INDEX workspace_templates_template_idx ON workspace_templates (template_id);
CREATE INDEX workspaces_vcs_connection_idx ON workspaces (vcs_connection_id) WHERE vcs_connection_id IS NOT NULL;
