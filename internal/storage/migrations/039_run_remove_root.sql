-- Removing a workspace's root template (the one it was created from) destroys its resources first: a
-- destroy run that, once applied (or with nothing to destroy), clears the root configuration. target
-- limits it to the root's own resources when added templates stay; NULL destroys everything.
ALTER TABLE runs ADD COLUMN remove_root boolean NOT NULL DEFAULT false, ADD COLUMN target jsonb;
