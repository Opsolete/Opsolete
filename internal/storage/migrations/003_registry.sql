-- Private module registry. Versions are immutable once uploaded.
-- shortcut: archives live in Postgres (capped at 20 MB each); move to blob storage if the registry grows large.
CREATE TABLE modules (
    namespace   text NOT NULL,
    name        text NOT NULL,
    system      text NOT NULL,
    version     text NOT NULL,          -- canonical semver, no "v" prefix
    archive     bytea NOT NULL,         -- .tar.gz with the module at its root
    variables   jsonb NOT NULL,         -- tfconfig variables, read at upload time
    uploaded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name, system, version)
);

-- Tokens for Terraform clients (TF_TOKEN_<host>) and pipelines; only the SHA-256 is stored.
CREATE TABLE api_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Single row: HMAC key for short-lived signed archive URLs (survives restarts, shared by replicas).
CREATE TABLE registry_signing_key (
    id  int PRIMARY KEY CHECK (id = 1),
    key bytea NOT NULL
);
