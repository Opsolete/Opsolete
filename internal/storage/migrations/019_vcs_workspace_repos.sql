-- Repositories workspaces may link through a connection: path.Match patterns such as acme/infra-*,
-- one per line. Empty: none, the connection only publishes registry modules.
-- Connections that existed keep allowing every repository their token sees.
ALTER TABLE vcs_connections ADD COLUMN workspace_repos text NOT NULL DEFAULT '';
UPDATE vcs_connections SET workspace_repos = '*/*';
