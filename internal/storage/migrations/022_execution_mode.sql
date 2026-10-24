-- Execution mode (as in HCP Terraform): remote = Opsolete runners plan and apply; local = Terraform
-- runs on the user's machine (the cloud block) and Opsolete only stores the state. Each mode refuses
-- the other's writes: local workspaces start no runs, remote ones give the CLI no state lock.
ALTER TABLE workspaces ADD COLUMN execution_mode text NOT NULL DEFAULT 'remote' CHECK (execution_mode IN ('remote', 'local'));
-- Workspaces that never had a run were made by (or for) the cloud block: keep the CLI working there.
UPDATE workspaces w SET execution_mode = 'local' WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.workspace_id = w.id);
