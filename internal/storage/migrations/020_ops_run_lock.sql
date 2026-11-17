-- The product is OPS now: a state lock's run is state_lock's OPSRun key, formerly OpsoleteRun.
UPDATE workspaces SET state_lock = (state_lock - 'OpsoleteRun') || jsonb_build_object('OPSRun', state_lock->'OpsoleteRun')
WHERE state_lock ? 'OpsoleteRun';
