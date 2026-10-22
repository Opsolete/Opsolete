-- Templates added to a workspace, any number of them: each is a module block (module.<name>) next to
-- the workspace's own configuration (the template it was created from, or its VCS branch), in its one state.
CREATE TABLE workspace_templates (
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          text NOT NULL, -- the module call name; the same template may be added under several names
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    inputs        jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, name)
);
