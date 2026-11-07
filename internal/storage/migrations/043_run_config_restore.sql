-- A run that changes a workspace's configuration (Configure…, adding or changing a template, a
-- template rollout) remembers it: [{module, before, after}], module "" the root, before null for an
-- added template. If the run ends before its apply started (discarded, canceled, or errored while
-- queued or planning), the configuration goes back to before, so the workspace never claims a version
-- that wasn't applied; not after its apply started: the state may already hold part of the new one.
-- A use changed since (no longer after) is left alone. Every way a run ends sets its status, so a
-- trigger covers them all: decide, CancelRun, FinishJob, ReapExpiredLeases.
ALTER TABLE runs ADD COLUMN config_changes jsonb;

-- run_config_snapshot is one use's configuration, null if there is none (an added template not added yet).
CREATE FUNCTION run_config_snapshot(ws uuid, module text) RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN module = '' THEN
        (SELECT jsonb_build_object('template_id', template_id, 'module_source', module_source, 'module_ref', module_ref, 'inputs', inputs)
            FROM workspaces WHERE id = ws)
    ELSE
        (SELECT jsonb_build_object('template_id', template_id, 'module_source', module_source, 'module_ref', module_ref, 'inputs', inputs)
            FROM workspace_templates WHERE workspace_id = ws AND name = module)
    END
$$;

CREATE FUNCTION runs_restore_config() RETURNS trigger LANGUAGE plpgsql AS $$
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
            IF c->>'module' = '' THEN
                UPDATE workspaces SET template_id = b->>'template_id', module_source = b->>'module_source',
                    module_ref = b->>'module_ref', inputs = b->'inputs', updated_at = now()
                WHERE id = NEW.workspace_id;
            ELSIF b IS NULL OR b = 'null'::jsonb THEN
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
CREATE TRIGGER runs_restore_config BEFORE UPDATE OF status ON runs FOR EACH ROW
    WHEN (NEW.config_changes IS NOT NULL AND NEW.status IS DISTINCT FROM OLD.status)
    EXECUTE FUNCTION runs_restore_config();
