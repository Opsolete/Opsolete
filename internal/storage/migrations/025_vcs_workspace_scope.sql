-- Workspaces scope a connection by workspace, as variable sets do, not by repository pattern:
-- '' none (it only publishes modules), 'all' every workspace, 'selected' those in vcs_connection_workspaces.
ALTER TABLE vcs_connections ADD COLUMN workspace_scope text NOT NULL DEFAULT ''
    CHECK (workspace_scope IN ('', 'all', 'selected'));

CREATE TABLE vcs_connection_workspaces (
    connection_id uuid NOT NULL REFERENCES vcs_connections(id) ON DELETE CASCADE,
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    PRIMARY KEY (connection_id, workspace_id)
);

-- Pattern-scoped connections keep the workspaces already linked through them.
UPDATE vcs_connections SET workspace_scope = CASE workspace_repos WHEN '' THEN '' WHEN '*/*' THEN 'all' ELSE 'selected' END;
INSERT INTO vcs_connection_workspaces (connection_id, workspace_id)
SELECT w.vcs_connection_id, w.id FROM workspaces w JOIN vcs_connections c ON c.id = w.vcs_connection_id
WHERE c.workspace_scope = 'selected';

ALTER TABLE vcs_connections DROP COLUMN workspace_repos;
