-- Workspaces no longer need a template, HCP style: one can be empty, hold only a state (uploaded),
-- only variables, or take its configuration from a VCS repository branch.
ALTER TABLE workspaces ALTER COLUMN template_id DROP NOT NULL, ALTER COLUMN module_ref DROP NOT NULL;
ALTER TABLE workspaces
    ADD COLUMN vcs_connection_id uuid REFERENCES vcs_connections(id) ON DELETE SET NULL,
    ADD COLUMN vcs_repo   text, -- owner/repo (GitHub) or project/repo (Azure DevOps)
    ADD COLUMN vcs_branch text;
-- The commit a VCS run planned, so its apply phase fetches the same configuration.
ALTER TABLE runs ADD COLUMN vcs_commit text;
