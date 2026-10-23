-- Removing a workspace's last added template destroys its resources first: a destroy run that,
-- once applied (or with nothing to destroy), removes this added template.
ALTER TABLE runs ADD COLUMN remove_template text;
