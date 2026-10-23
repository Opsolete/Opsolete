-- Variables replace runner pools: any runner runs any workspace, with the
-- environment (credentials such as ARM_CLIENT_SECRET) and Terraform variables the server hands it
-- per job. They come from the workspace itself and from variable sets: global, or attached to workspaces.

-- Runs queued for a pool would now go to any runner, without that pool's credentials.
WITH r AS (
    UPDATE runs SET status = 'errored', updated_at = now()
    WHERE status IN ('plan_queued', 'apply_queued')
        AND workspace_id IN (SELECT id FROM workspaces WHERE identity <> '')
    RETURNING id, workspace_id
)
INSERT INTO run_status_history (run_id, status, message)
SELECT r.id, 'errored', 'runner pools were removed: give the workspace the "' || w.identity ||
    '" credentials as variables (or a variable set), then start a new run'
FROM r JOIN workspaces w ON w.id = r.workspace_id;

ALTER TABLE workspaces DROP COLUMN identity;
ALTER TABLE api_tokens DROP COLUMN identity;

CREATE TABLE variable_sets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    global      boolean NOT NULL DEFAULT false, -- applies to every workspace
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE variable_set_workspaces (
    set_id       uuid NOT NULL REFERENCES variable_sets(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    PRIMARY KEY (set_id, workspace_id)
);
CREATE INDEX variable_set_workspaces_ws_idx ON variable_set_workspaces (workspace_id);

-- A variable belongs to exactly one workspace or one variable set.
CREATE TABLE variables (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid REFERENCES workspaces(id) ON DELETE CASCADE,
    set_id       uuid REFERENCES variable_sets(id) ON DELETE CASCADE,
    category     text NOT NULL CHECK (category IN ('env', 'terraform')),
    key          text NOT NULL,
    value        bytea NOT NULL,  -- AES-GCM sealed with SECRET_KEY
    sensitive    boolean NOT NULL DEFAULT false, -- write-only: never shown again
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((workspace_id IS NULL) <> (set_id IS NULL)),
    UNIQUE (workspace_id, category, key),
    UNIQUE (set_id, category, key)
);
