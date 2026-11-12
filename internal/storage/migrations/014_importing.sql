-- importing: the module imports what exists (the template's import switch, app.ImportInput, is added to its inputs
-- for the runner only): a property of the configuration, not a stored form value, so a promotion or a new version
-- doesn't carry it by accident. Upgrade: the switch moves out of the inputs into the flag.
ALTER TABLE configuration_modules ADD COLUMN importing boolean NOT NULL DEFAULT false;
UPDATE configuration_modules SET importing = (inputs -> 'existing' = 'true'::jsonb), inputs = inputs - 'existing'
WHERE jsonb_typeof(inputs -> 'existing') = 'boolean';

CREATE OR REPLACE VIEW workspace_templates AS
SELECT w.id AS workspace_id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs, m.follows, m.importing
FROM workspaces w JOIN configuration_modules m ON m.cv_id = COALESCE(
    (SELECT r.cv_id FROM runs r WHERE r.workspace_id = w.id AND run_active(r.status)), w.current_cv_id);
