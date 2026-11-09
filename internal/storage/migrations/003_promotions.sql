-- always_confirm: the run's plan waits for a human even with auto-apply (a promotion, storage.Promote;
-- a rollout's run does the same through rollout_id).
ALTER TABLE runs ADD COLUMN always_confirm boolean NOT NULL DEFAULT false;

-- remove_templates (replaces remove_template): a destroy run that removes those added templates
-- (-target module.<name> each); applied, their rows are deleted (storage/runs.go setStatus).
ALTER TABLE runs ADD COLUMN remove_templates text[];
UPDATE runs SET remove_templates = ARRAY[remove_template] WHERE remove_template IS NOT NULL;
ALTER TABLE runs DROP COLUMN remove_template; -- and its CHECK
ALTER TABLE runs ADD CHECK (remove_templates IS NULL OR action = 'destroy');

-- promotion: a promotion's first run (a destroy of the templates it removes or replaces) carries the
-- rest of it (storage.pendingPromotion); applied, setStatus queues its apply run. Dropped when it ends.
ALTER TABLE runs ADD COLUMN promotion jsonb;
ALTER TABLE runs ADD CHECK (promotion IS NULL OR remove_templates IS NOT NULL);
