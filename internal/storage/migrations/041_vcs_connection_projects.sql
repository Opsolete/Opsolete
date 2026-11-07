-- A connection's 'selected' scope also allows every workspace of these projects (a new one too),
-- next to the workspaces in vcs_connection_workspaces.
CREATE TABLE vcs_connection_projects (
    connection_id uuid NOT NULL REFERENCES vcs_connections(id) ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (connection_id, project_id)
);
CREATE INDEX vcs_connection_projects_project_idx ON vcs_connection_projects (project_id);
