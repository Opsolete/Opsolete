-- VCS providers (GitHub, Azure DevOps): registry modules published from a repository, HCP-style.
-- Every semver tag in the repo becomes an (immutable) module version.
CREATE TABLE vcs_connections (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    provider   text NOT NULL CHECK (provider IN ('github', 'azure-devops')),
    base_url   text NOT NULL,   -- API root: https://api.github.com, https://dev.azure.com/<org>, ...
    token      bytea NOT NULL,  -- access token, AES-GCM sealed with SECRET_KEY; never shown again
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Deleting a connection unlinks its modules; versions already published stay in the registry.
CREATE TABLE vcs_modules (
    namespace     text NOT NULL,
    name          text NOT NULL,
    system        text NOT NULL,
    connection_id uuid NOT NULL REFERENCES vcs_connections(id) ON DELETE CASCADE,
    repo          text NOT NULL, -- owner/repo (GitHub) or project/repo (Azure DevOps)
    synced_at     timestamptz,
    sync_error    text NOT NULL DEFAULT '',
    PRIMARY KEY (namespace, name, system)
);
