-- The audit log: an HMAC chain, written under an advisory lock so ids follow it without gaps.

-- name: LockAudit :exec
SELECT pg_advisory_xact_lock(4242424243);

-- name: LastAuditEntry :one
SELECT id, hash FROM audit_log ORDER BY id DESC LIMIT 1;

-- name: InsertAuditEntry :exec
INSERT INTO audit_log (id, at, actor, action, detail, prev_hash, hash)
VALUES (sqlc.arg(id)::bigint, sqlc.arg(at)::timestamptz, sqlc.arg(actor)::text, sqlc.arg(action)::text, sqlc.arg(detail)::json,
    sqlc.arg(prev_hash)::bytea, sqlc.arg(hash)::bytea);

-- name: AuditLog :many
-- access: sign-ins and requests refused with 401/403; else everything else (the changes)
SELECT id, at, actor, action, detail, hash FROM audit_log
WHERE (action = 'GET /auth/callback' OR coalesce(detail->'request'->>'status', '') IN ('401', '403')) = sqlc.arg(access)::boolean
ORDER BY id DESC LIMIT sqlc.arg(lim)::integer;

-- name: AuditChain :many
SELECT * FROM audit_log ORDER BY id;
