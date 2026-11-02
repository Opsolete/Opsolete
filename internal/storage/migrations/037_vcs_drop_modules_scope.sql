-- Any connection may publish registry modules: publishing is admin-only, so the switch only kept admins from admins.
ALTER TABLE vcs_connections DROP COLUMN modules;
