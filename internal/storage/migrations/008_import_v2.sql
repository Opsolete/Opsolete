-- Importing existing resources, rebuilt (docs/resource-import.md): a template's module imports what exists itself
-- (import blocks switched by an input), so scans and the imports Opsolete wrote are gone. found: what a plan that
-- imports found, per template module, the inputs and their real values (runnerapi.FinishRequest.Found), for the
-- form's "Use found values". A plan's imports (runs.plan) feed the import guard.
DELETE FROM runs WHERE action = 'scan';
ALTER TABLE runs DROP CONSTRAINT runs_action_check;
ALTER TABLE runs ADD CONSTRAINT runs_action_check CHECK (action IN ('apply', 'destroy', 'refresh', 'assess', 'plan'));
ALTER TABLE runs DROP COLUMN scan_resource_group, DROP COLUMN candidates, DROP COLUMN inventory, DROP COLUMN imports;
ALTER TABLE runs ADD COLUMN found jsonb;
