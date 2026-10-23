-- Refresh-only runs, replace runs (apply runs with -replace addresses) and drift detection:
-- 'assess' runs are scheduled refresh-only plans that are never applied; they end no_changes or drifted.
ALTER TABLE runs DROP CONSTRAINT runs_action_check,
    ADD CONSTRAINT runs_action_check CHECK (action IN ('apply', 'destroy', 'refresh', 'assess')),
    ADD COLUMN replace jsonb;
ALTER TABLE workspaces ADD COLUMN drift_detection boolean NOT NULL DEFAULT false;
