-- Remote state sharing (as in HCP Terraform): a workspace's state can be read by the runs of every
-- other workspace (global_remote_state), or of the workspaces listed here. Default: by none.
ALTER TABLE workspaces ADD COLUMN global_remote_state boolean NOT NULL DEFAULT false;
CREATE TABLE remote_state_consumers (
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    consumer_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    PRIMARY KEY (workspace_id, consumer_id),
    CHECK (workspace_id <> consumer_id)
);
