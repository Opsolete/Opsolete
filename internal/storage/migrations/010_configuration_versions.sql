-- Configuration versions (docs/configuration-versions.md): a workspace's templates are an immutable version; a run
-- plans one (runs.cv_id) and the workspace points at the one whose apply started (workspaces.current_cv_id). A run
-- that ends without its apply changes nothing, so config_changes and its restore go.
CREATE TABLE configuration_versions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    parent_id    uuid REFERENCES configuration_versions ON DELETE SET NULL, -- the current version it was made from
    source       text NOT NULL CHECK (source IN ('form', 'promote', 'rollout', 'import', 'remove', 'upgrade')),
    created_by   uuid REFERENCES users,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX configuration_versions_workspace_idx ON configuration_versions (workspace_id);

-- configuration_modules: a version's template modules, each the module block module.<name>, pinned to a module
-- version. inputs: the form values; sensitive ones are {"sealed": "<base64>"} (rotate-key re-seals them here).
CREATE TABLE configuration_modules (
    cv_id         uuid NOT NULL REFERENCES configuration_versions ON DELETE CASCADE,
    name          text NOT NULL,
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    inputs        jsonb NOT NULL,
    PRIMARY KEY (cv_id, name)
);
CREATE INDEX configuration_modules_template_idx ON configuration_modules (template_id);

ALTER TABLE workspaces ADD COLUMN current_cv_id uuid REFERENCES configuration_versions ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN cv_id uuid REFERENCES configuration_versions ON DELETE SET NULL;

-- Upgrade (source 'upgrade'): each workspace's templates become its current version, and every run since an
-- active run's change plans them. An active run that changed templates (config_changes) wrote them already: its
-- version is the templates as they are, the current one has its befores put back, as its restore would.
DO $$
DECLARE
    w record;
    cur uuid;
    pending uuid;
    c jsonb;
BEGIN
    FOR w IN SELECT ws.id, (SELECT r.id FROM runs r WHERE r.workspace_id = ws.id AND run_active(r.status)) AS run,
            (SELECT r.config_changes FROM runs r WHERE r.workspace_id = ws.id AND run_active(r.status)) AS changes
        FROM workspaces ws WHERE EXISTS (SELECT 1 FROM workspace_templates t WHERE t.workspace_id = ws.id)
            OR EXISTS (SELECT 1 FROM runs r WHERE r.workspace_id = ws.id AND run_active(r.status) AND r.config_changes IS NOT NULL)
    LOOP
        INSERT INTO configuration_versions (workspace_id, source) VALUES (w.id, 'upgrade') RETURNING id INTO cur;
        INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
        SELECT cur, t.name, t.template_id, t.module_source, t.module_ref, t.inputs FROM workspace_templates t WHERE t.workspace_id = w.id;
        pending := cur;
        IF w.changes IS NOT NULL AND jsonb_array_length(w.changes) > 0 THEN
            -- the templates as they are are the run's (pending); a new current one has its befores put back
            INSERT INTO configuration_versions (workspace_id, source) VALUES (w.id, 'upgrade') RETURNING id INTO cur;
            UPDATE configuration_versions SET parent_id = cur WHERE id = pending;
            INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
            SELECT cur, m.name, m.template_id, m.module_source, m.module_ref, m.inputs FROM configuration_modules m
            WHERE m.cv_id = pending AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(w.changes) x WHERE x->>'module' = m.name);
            FOR c IN SELECT * FROM jsonb_array_elements(w.changes) LOOP
                IF c->'before' IS NOT NULL AND jsonb_typeof(c->'before') = 'object' THEN
                    INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
                    VALUES (cur, c->>'module', c->'before'->>'template_id', c->'before'->>'module_source',
                        c->'before'->>'module_ref', c->'before'->'inputs');
                END IF;
            END LOOP;
        END IF;
        UPDATE workspaces SET current_cv_id = cur WHERE id = w.id;
        UPDATE runs SET cv_id = pending WHERE id = w.run;
    END LOOP;
END $$;

-- workspace_templates: the templates the workspace has or is getting: the version its active run plans, else its
-- current one. A run that ends without its apply leaves the current one, so they're back without an undo. Writes
-- create versions (storage/configversions.go).
DROP VIEW workspace_list; -- counts them; recreated below as it was
DROP TABLE workspace_templates;
CREATE VIEW workspace_templates AS
SELECT w.id AS workspace_id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs
FROM workspaces w JOIN configuration_modules m ON m.cv_id = COALESCE(
    (SELECT r.cv_id FROM runs r WHERE r.workspace_id = w.id AND run_active(r.status)), w.current_cv_id);

CREATE VIEW workspace_list AS
SELECT w.id, w.name, w.project_id, p.name AS project_name, w.created_by, COALESCE(u.email, '') AS created_by_email,
    w.auto_apply, w.drift_detection, w.global_remote_state, w.execution_mode, w.created_at,
    vcs.connection_id AS vcs_connection_id, COALESCE(vcs.repo, '') AS vcs_repo, COALESCE(vcs.branch, '') AS vcs_branch,
    COALESCE(vcs.trigger, false) AS vcs_trigger, COALESCE(vcs.pull_requests, false) AS vcs_pull_requests,
    COALESCE(vcs.tags_regex, '') AS vcs_tags_regex, COALESCE(vcs.tag_baseline, '') AS vcs_tag_baseline,
    (SELECT count(*) FROM workspace_templates t WHERE t.workspace_id = w.id)::bigint AS templates,
    COALESCE((SELECT a.status = 'drifted' AND NOT EXISTS (SELECT 1 FROM runs x WHERE x.workspace_id = w.id
            AND x.status = 'applied' AND x.updated_at > a.updated_at) FROM runs a
        WHERE a.workspace_id = w.id AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted')
        ORDER BY a.created_at DESC LIMIT 1), false)::boolean AS drifted
FROM workspaces w
JOIN projects p ON p.id = w.project_id
LEFT JOIN users u ON u.id = w.created_by
LEFT JOIN workspace_vcs vcs ON vcs.workspace_id = w.id;

ALTER TABLE runs DROP COLUMN config_changes;
DROP FUNCTION template_config(text, text, text, jsonb);
