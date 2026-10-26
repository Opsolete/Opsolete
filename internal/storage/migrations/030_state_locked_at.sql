-- When the current state lock was taken, so locks no run or CLI can release (a rollback whose
-- server died) expire after STALE_LOCK_TIMEOUT. Locks held now count from the upgrade.
ALTER TABLE workspaces ADD COLUMN state_locked_at timestamptz;
UPDATE workspaces SET state_locked_at = now() WHERE state_lock IS NOT NULL;
