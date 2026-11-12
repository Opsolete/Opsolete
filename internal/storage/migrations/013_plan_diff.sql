-- plan_diff: a plan's attribute changes per resource (runnerapi.ResourceDiff, values masked), for the run page and
-- the API. Kept apart from plan (counts and addresses), which every run list reads.
ALTER TABLE runs ADD COLUMN plan_diff jsonb;
