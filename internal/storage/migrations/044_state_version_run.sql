-- The run whose Terraform wrote a state version (null for the CLI, the cloud API and rollbacks).
-- Terraform persists intermediate snapshots during a long apply (TF_STATE_PERSIST_INTERVAL); like
-- HCP Terraform, the ones superseded by the run's final state are deleted when the run ends.
-- No foreign key: runs are only deleted with their workspace, which deletes its state versions too.
ALTER TABLE state_versions ADD COLUMN run_id uuid;
CREATE INDEX state_versions_run_idx ON state_versions (run_id) WHERE run_id IS NOT NULL;
