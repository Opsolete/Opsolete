-- Runner pools: a workspace runs as one named cloud identity, and only runners whose token
-- belongs to that pool may claim its runs. '' is the default pool.
ALTER TABLE workspaces ADD COLUMN identity text NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN identity text NOT NULL DEFAULT '';

-- Team access: IdP groups (the OIDC "groups" claim, recorded at login) granted per workspace.
ALTER TABLE users ADD COLUMN groups text[] NOT NULL DEFAULT '{}';
CREATE TABLE workspace_access (
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    group_name   text NOT NULL,
    level        text NOT NULL CHECK (level IN ('read', 'operate')),
    PRIMARY KEY (workspace_id, group_name)
);

-- Graceful cancel: the runner interrupts Terraform, which still saves state, then reports.
ALTER TABLE runs ADD COLUMN cancel_requested boolean NOT NULL DEFAULT false;
