-- Outputs belong to a state version, extracted from the state file when it is
-- saved; sensitive values are stored as "(sensitive)".
ALTER TABLE state_versions ADD COLUMN outputs jsonb;
-- Existing workspaces: the current version gets the outputs of the last successful apply.
UPDATE state_versions v SET outputs = r.outputs
FROM (SELECT DISTINCT ON (workspace_id) id, workspace_id FROM state_versions ORDER BY workspace_id, id DESC) cur,
     (SELECT DISTINCT ON (workspace_id) workspace_id, outputs FROM runs WHERE status = 'applied' ORDER BY workspace_id, created_at DESC) r
WHERE v.id = cur.id AND r.workspace_id = cur.workspace_id;
