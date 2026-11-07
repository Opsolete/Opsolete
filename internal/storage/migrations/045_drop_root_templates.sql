-- No more root templates: a workspace created from a template gets it as an added template
-- (module.<name>), like every template since, so each brings its own providers. The root
-- configuration is a VCS branch or nothing. A root template's resources live at root addresses:
-- turning it into an added template would make Terraform replace them all, so refuse instead.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM workspaces WHERE template_id IS NOT NULL OR module_source IS NOT NULL) THEN
        RAISE EXCEPTION 'workspaces with a root template exist (workspaces.template_id): remove their root template first, with the version before this one';
    END IF;
END $$;

-- Only the root template set these: its removal (runs.remove_root, the -target of its resources).
ALTER TABLE runs DROP COLUMN remove_root, DROP COLUMN target;
ALTER TABLE workspaces DROP COLUMN template_id, DROP COLUMN module_source, DROP COLUMN module_ref, DROP COLUMN inputs;

-- run_config_snapshot and runs_restore_config (043) without the root (module '').
CREATE OR REPLACE FUNCTION run_config_snapshot(ws uuid, module text) RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT jsonb_build_object('template_id', template_id, 'module_source', module_source, 'module_ref', module_ref, 'inputs', inputs)
        FROM workspace_templates WHERE workspace_id = ws AND name = module
$$;

CREATE OR REPLACE FUNCTION runs_restore_config() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    c jsonb;
    b jsonb;
BEGIN
    IF NEW.status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying') THEN
        RETURN NEW; -- not ended
    END IF;
    IF NEW.status IN ('discarded', 'canceled', 'errored') AND OLD.status <> 'applying' THEN
        FOR c IN SELECT * FROM jsonb_array_elements(NEW.config_changes) LOOP
            CONTINUE WHEN run_config_snapshot(NEW.workspace_id, c->>'module') IS DISTINCT FROM c->'after';
            b := c->'before';
            IF b IS NULL OR b = 'null'::jsonb THEN
                DELETE FROM workspace_templates WHERE workspace_id = NEW.workspace_id AND name = c->>'module';
            ELSE
                UPDATE workspace_templates SET template_id = b->>'template_id', module_source = b->>'module_source',
                    module_ref = b->>'module_ref', inputs = b->'inputs'
                WHERE workspace_id = NEW.workspace_id AND name = c->>'module';
            END IF;
        END LOOP;
    END IF;
    NEW.config_changes := NULL; -- ended: nothing left to restore (and no copy of sealed inputs kept)
    RETURN NEW;
END $$;
