-- Runs (docs/configuration-versions.md, phases C and E): one confirmation rule and promotions as records.
--
-- promotions: a promotion's target templates are a configuration version (cv_id), applied by apply_run_id; one
-- that removes or replaces a deployed template destroys them first (destroy_run_id), and the destroy applied
-- queues the apply run (storage.continuePromotion). Replaces runs.promotion (a sealed copy of the templates
-- RotateKey couldn't re-seal) and runs.always_confirm (a promotion's runs are its destroy and apply runs).
CREATE TABLE promotions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target_workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    source              text NOT NULL,  -- the source workspace's name, for history and audit
    cv_id               uuid REFERENCES configuration_versions ON DELETE SET NULL,
    created_by          uuid REFERENCES users,
    actor               text NOT NULL DEFAULT '', -- created_by's email, for the chained apply run's audit entry
    destroy_run_id      uuid UNIQUE REFERENCES runs ON DELETE SET NULL,
    apply_run_id        uuid UNIQUE REFERENCES runs ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- confirm_reason: why the run's plan waits for a human (storage.confirmReason); set when it does.
ALTER TABLE runs ADD COLUMN confirm_reason text;

-- Upgrade: a promotion's apply runs (always_confirm) and an in-flight destroy run's pending promotion.
INSERT INTO promotions (target_workspace_id, source, cv_id, apply_run_id, created_at)
SELECT r.workspace_id, '', r.cv_id, r.id, r.created_at FROM runs r WHERE r.always_confirm AND r.action = 'apply';
DO $$
DECLARE
    r record;
    cv uuid;
BEGIN
    FOR r IN SELECT id, workspace_id, promotion FROM runs WHERE promotion IS NOT NULL LOOP
        INSERT INTO configuration_versions (workspace_id, parent_id, source)
        SELECT r.workspace_id, w.current_cv_id, 'promote' FROM workspaces w WHERE w.id = r.workspace_id RETURNING id INTO cv;
        INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
        SELECT cv, t->>'Name', t->>'TemplateID', t->>'ModuleSource', t->>'ModuleRef', COALESCE(t->'Inputs', '{}'::jsonb)
        FROM jsonb_array_elements(r.promotion->'templates') t;
        INSERT INTO promotions (target_workspace_id, source, cv_id, created_by, actor, destroy_run_id)
        VALUES (r.workspace_id, COALESCE(r.promotion->>'source', ''), cv, NULLIF(r.promotion->>'created_by', '00000000-0000-0000-0000-000000000000')::uuid,
            COALESCE(r.promotion->>'actor', ''), r.id);
    END LOOP;
END $$;
UPDATE runs r SET confirm_reason = CASE
        WHEN EXISTS (SELECT 1 FROM promotions p WHERE r.id IN (p.destroy_run_id, p.apply_run_id)) THEN 'promotion'
        WHEN r.rollout_id IS NOT NULL THEN 'rollout'
        WHEN r.action IN ('destroy', 'refresh') THEN r.action
        ELSE 'manual' END
WHERE r.status = 'needs_confirmation';

ALTER TABLE runs DROP COLUMN promotion, DROP COLUMN always_confirm;

CREATE OR REPLACE VIEW run_list AS
SELECT r.id, r.workspace_id, r.action, r.status, r.replace, r.plan, r.outputs, r.runner, r.cancel_requested,
    COALESCE(r.vcs_commit, '') AS vcs_commit, COALESCE(r.vcs_pull_request, 0)::integer AS vcs_pull_request,
    COALESCE(cu.email, '') AS created_by_email, COALESCE(fu.email, '') AS confirmed_by_email, r.created_at, r.updated_at,
    r.log_removed_at, r.log_bytes, COALESCE(r.confirm_reason, '') AS confirm_reason
FROM runs r
LEFT JOIN users cu ON cu.id = r.created_by
LEFT JOIN users fu ON fu.id = r.confirmed_by;
