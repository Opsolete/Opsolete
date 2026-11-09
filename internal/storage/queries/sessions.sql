-- Browser sessions: the cookie's token, stored as its SHA-256.

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, admin_email, admin_groups, user_agent, ip, expires_at)
VALUES (sqlc.arg(token_hash)::bytea, sqlc.arg(user_id)::uuid, sqlc.arg(admin_email)::text, sqlc.arg(admin_groups)::text[],
    sqlc.arg(user_agent)::text, sqlc.arg(ip)::text, now() + make_interval(secs => sqlc.arg(ttl_secs)::float8));

-- name: SessionUser :one
-- every request's lookup, one statement: last_seen_at moves at most once a minute, to keep writes cheap
WITH touch AS (
    UPDATE sessions SET last_seen_at = now()
    WHERE token_hash = sqlc.arg(token_hash)::bytea AND expires_at > now() AND last_seen_at < now() - interval '1 minute'
)
SELECT s.id, s.user_id, u.email, s.admin_email, s.admin_groups
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = sqlc.arg(token_hash)::bytea AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = sqlc.arg(token_hash)::bytea;

-- name: DeleteUserSessions :execrows
DELETE FROM sessions WHERE user_id = sqlc.arg(user_id)::uuid;

-- name: UserSessions :many
SELECT id, user_agent, ip, created_at, last_seen_at, expires_at FROM sessions
WHERE user_id = sqlc.arg(user_id)::uuid AND expires_at > now()
ORDER BY last_seen_at DESC;

-- name: PruneSessions :execrows
DELETE FROM sessions WHERE expires_at <= now();
