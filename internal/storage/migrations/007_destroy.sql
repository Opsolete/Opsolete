-- What the next/last run of a deployment does. A destroy re-queues the same row (same state key);
-- a successful destroy ends in status 'destroyed'.
ALTER TABLE deployments
    ADD COLUMN action text NOT NULL DEFAULT 'apply' CHECK (action IN ('apply', 'destroy'));
