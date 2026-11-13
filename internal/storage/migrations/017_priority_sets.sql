-- Priority variable sets: their variables win over a workspace's own and every other set's, wherever
-- the set applies (variable_set_rank still decides where). For values an admin enforces, e.g. a provider's region.
ALTER TABLE variable_sets ADD COLUMN priority boolean NOT NULL DEFAULT false;

CREATE OR REPLACE VIEW variable_set_list AS
SELECT vs.id, vs.name, vs.description, vs.global, vs.created_at,
    ARRAY(SELECT w.name FROM variable_set_workspaces a JOIN workspaces w ON w.id = a.workspace_id
        WHERE a.set_id = vs.id ORDER BY w.name)::text[] AS workspaces,
    ARRAY(SELECT p.name FROM variable_set_projects a JOIN projects p ON p.id = a.project_id
        WHERE a.set_id = vs.id ORDER BY p.name)::text[] AS projects,
    vs.priority
FROM variable_sets vs;
