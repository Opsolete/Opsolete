-- Form2Stack is the Terraform state backend (http backend), one state per workspace (deployment).
-- State files live in blob storage (STATE_STORE). Postgres keeps their version list, the state lock,
-- and per version an index of its resources (names and types only) derived from that version.
CREATE TABLE state_versions (
    id            bigserial PRIMARY KEY,
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    serial        bigint NOT NULL,
    lineage       text NOT NULL,
    blob_key      text NOT NULL UNIQUE,
    md5           text NOT NULL,
    size          bigint NOT NULL,
    indexed       boolean NOT NULL DEFAULT false, -- resources below were extracted from this version
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX state_versions_deployment_idx ON state_versions (deployment_id, id DESC);

CREATE TABLE state_version_resources (
    state_version_id bigint NOT NULL REFERENCES state_versions(id) ON DELETE CASCADE,
    address          text NOT NULL,
    type             text NOT NULL,
    name             text NOT NULL,
    provider         text NOT NULL,
    module           text NOT NULL DEFAULT '',
    PRIMARY KEY (state_version_id, address)
);
CREATE INDEX state_version_resources_type_idx ON state_version_resources (type);

-- Terraform's LockInfo while a run holds the state lock; cleared when the run ends.
ALTER TABLE deployments ADD COLUMN state_lock jsonb;
