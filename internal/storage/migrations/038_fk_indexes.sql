-- Indexes for lookups and ON DELETE checks that scanned whole tables: a run's status history
-- (run pages, metrics, deleting runs), rollback links, and the cascades from deleting a workspace.
CREATE INDEX run_status_history_run_idx ON run_status_history (run_id, id);
CREATE INDEX state_versions_rollback_of_idx ON state_versions (rollback_of) WHERE rollback_of IS NOT NULL;
CREATE INDEX remote_state_consumers_consumer_idx ON remote_state_consumers (consumer_id);
CREATE INDEX vcs_connection_workspaces_ws_idx ON vcs_connection_workspaces (workspace_id);
-- No query filters resources by type.
DROP INDEX state_version_resources_type_idx;
