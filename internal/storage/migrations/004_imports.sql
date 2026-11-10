-- imports: existing resources an apply run adopts, [{"to": address, "id": resource ID}] (runnerapi.Import), written
-- as import blocks in both phases. Such a run always waits for a human (always_confirm), and FinishJob ends it
-- errored if its plan would replace or delete anything.
ALTER TABLE runs ADD COLUMN imports jsonb;
ALTER TABLE runs ADD CHECK (imports IS NULL OR action = 'apply');
