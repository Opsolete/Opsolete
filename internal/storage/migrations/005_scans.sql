-- scan: a run that finds existing resources the catalog's templates can take over (runner.scan): plan phase
-- only, never applied, no state lock. scan_resource_group: the resource group it searches ('' or NULL: the
-- whole subscription); candidates: what templates can import ([]runnerapi.Candidate); inventory: every
-- resource it saw ([]runnerapi.Found).
ALTER TABLE runs DROP CONSTRAINT runs_action_check;
ALTER TABLE runs ADD CONSTRAINT runs_action_check CHECK (action IN ('apply', 'destroy', 'refresh', 'assess', 'plan', 'scan'));
ALTER TABLE runs ADD COLUMN scan_resource_group text;
ALTER TABLE runs ADD COLUMN candidates jsonb;
ALTER TABLE runs ADD COLUMN inventory jsonb;
ALTER TABLE runs ADD CHECK (scan_resource_group IS NULL OR action = 'scan');

-- resource_id: the resource's id attribute (runnerapi.Resource.ID): scans mark what is managed already, and an
-- import of a managed resource is refused. A state indexed before has none until its workspace's next run.
ALTER TABLE state_version_resources ADD COLUMN resource_id text;
CREATE INDEX state_version_resources_id_idx ON state_version_resources (lower(resource_id)) WHERE resource_id IS NOT NULL;
