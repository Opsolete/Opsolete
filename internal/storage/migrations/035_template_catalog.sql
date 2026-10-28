-- The template catalog's Git repository (one row, or none: TEMPLATES_DIR), with the last loaded
-- commit's template files, so startup and the forms don't depend on the provider being up.
CREATE TABLE template_catalog (
    id            boolean PRIMARY KEY DEFAULT true CHECK (id),
    connection_id uuid NOT NULL REFERENCES vcs_connections(id), -- no cascade: deleting it would empty the catalog
    repo          text NOT NULL,
    branch        text NOT NULL,
    dir           text NOT NULL DEFAULT '', -- folder of the *.json files; '' = the repository root
    mode          text NOT NULL CHECK (mode IN ('off', 'commit', 'pr')), -- what the editor's saves do
    commit_sha    text NOT NULL DEFAULT '',
    files         jsonb NOT NULL DEFAULT '{}', -- path -> file content, at commit_sha
    synced_at     timestamptz,
    sync_error    text NOT NULL DEFAULT ''
);

-- Module uploads in the registry (admins), formerly TEMPLATE_EDITOR.
ALTER TABLE organization ADD COLUMN module_uploads boolean NOT NULL DEFAULT false;
