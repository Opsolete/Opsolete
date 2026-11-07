-- A template rollout moves the workspaces using a template to the module version it pinned when the
-- rollout started, in batches, retrying busy workspaces until all are done.
CREATE TABLE template_rollouts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE, -- NULL: every project
    batch_size    int NOT NULL DEFAULT 0 CHECK (batch_size >= 0),  -- workspaces with an open rollout run at a time; 0: no limit
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'done', 'stopped')),
    message       text NOT NULL DEFAULT '',
    created_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    resumed_at    timestamptz NOT NULL DEFAULT now(), -- runs that fail after this pause it
    ticked_at     timestamptz                         -- the last time an app instance worked on it
);
CREATE UNIQUE INDEX template_rollouts_open_idx ON template_rollouts (template_id) WHERE status IN ('active', 'paused');

-- A rollout's runs wait for confirmation even on auto-apply workspaces: a new module version may
-- replace or destroy resources, which someone should see first.
ALTER TABLE runs ADD COLUMN rollout_id uuid REFERENCES template_rollouts(id) ON DELETE SET NULL;
