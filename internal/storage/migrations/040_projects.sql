-- Projects group workspaces: every workspace is in exactly one, and grants and
-- variable sets on a project reach all of its workspaces. Workspace names stay unique instance-wide.
CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO projects (name) VALUES ('default');

-- New workspaces land in the default project; the reference keeps it from being deleted.
ALTER TABLE organization ADD COLUMN default_project_id uuid REFERENCES projects;
UPDATE organization SET default_project_id = (SELECT id FROM projects);
ALTER TABLE organization ALTER COLUMN default_project_id SET NOT NULL;

-- RESTRICT: only empty projects can be deleted.
ALTER TABLE workspaces ADD COLUMN project_id uuid REFERENCES projects ON DELETE RESTRICT;
UPDATE workspaces SET project_id = (SELECT default_project_id FROM organization);
ALTER TABLE workspaces ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX workspaces_project_idx ON workspaces (project_id);

CREATE TABLE project_access (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    group_name text NOT NULL,
    level      text NOT NULL CHECK (level IN ('read', 'operate')),
    PRIMARY KEY (project_id, group_name)
);

CREATE TABLE variable_set_projects (
    set_id     uuid NOT NULL REFERENCES variable_sets(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (set_id, project_id)
);
CREATE INDEX variable_set_projects_project_idx ON variable_set_projects (project_id);
