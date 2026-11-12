-- follows: a module's per-environment inputs that take the project's value even when the project's value lets
-- workspaces keep their own (keep_own): decided when the form was saved with the input locked to the project's
-- value (app.EnvironmentMap.Locked). It replaces comparing the stored value with the project's, which pinned a
-- workspace that followed the project to the old value as soon as the project changed it.
ALTER TABLE configuration_modules ADD COLUMN follows text[] NOT NULL DEFAULT '{}';

-- Upgrade: what the comparison decided until now (a stored value equal to a keep_own project value follows it).
UPDATE configuration_modules m SET follows = ARRAY(
    SELECT e.input_name FROM environment_values e
    WHERE e.project_id = w.project_id AND e.template_id = m.template_id AND e.keep_own
        AND m.inputs ->> e.input_name = e.value ORDER BY 1)
FROM configuration_versions cv JOIN workspaces w ON w.id = cv.workspace_id
WHERE cv.id = m.cv_id;

CREATE OR REPLACE VIEW workspace_templates AS
SELECT w.id AS workspace_id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs, m.follows
FROM workspaces w JOIN configuration_modules m ON m.cv_id = COALESCE(
    (SELECT r.cv_id FROM runs r WHERE r.workspace_id = w.id AND run_active(r.status)), w.current_cv_id);
