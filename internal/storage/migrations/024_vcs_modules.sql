-- Whether registry modules may be published from a connection's repositories (VCS provider scope, as in HCP Terraform).
-- On for existing connections: they could always publish. Linked modules keep syncing when it is turned off.
ALTER TABLE vcs_connections ADD COLUMN modules boolean NOT NULL DEFAULT true;
