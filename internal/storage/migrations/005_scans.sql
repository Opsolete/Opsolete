-- scan: a run that finds existing resources the catalog's templates can take over (runner.scan): plan phase
-- only, never applied, no state lock. scan_resource_group: the resource group it searches ('' or NULL: the
-- whole subscription); candidates: what it found ([]runnerapi.Candidate).
ALTER TABLE runs DROP CONSTRAINT runs_action_check;
ALTER TABLE runs ADD CONSTRAINT runs_action_check CHECK (action IN ('apply', 'destroy', 'refresh', 'assess', 'plan', 'scan'));
ALTER TABLE runs ADD COLUMN scan_resource_group text;
ALTER TABLE runs ADD COLUMN candidates jsonb;
ALTER TABLE runs ADD CHECK (scan_resource_group IS NULL OR action = 'scan');

-- resource_id: the resource's id attribute (runnerapi.Resource.ID), so a scan tells what is managed already.
-- NULL in versions indexed before.
ALTER TABLE state_version_resources ADD COLUMN resource_id text;
CREATE INDEX state_version_resources_id_idx ON state_version_resources (lower(resource_id)) WHERE resource_id IS NOT NULL;
