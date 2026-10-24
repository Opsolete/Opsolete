-- A rollback's state version is a copy of an older one: which, so the version list can say so.
ALTER TABLE state_versions ADD COLUMN rollback_of bigint REFERENCES state_versions(id) ON DELETE SET NULL;
