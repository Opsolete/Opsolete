-- Runners and their tokens (009_runners.sql).

-- name: CreateRunner :exec
INSERT INTO runners (name, pool, token_hash) VALUES (sqlc.arg(name)::text, sqlc.arg(pool)::text, sqlc.arg(token_hash)::bytea);

-- name: RegisterRunner :exec
-- A token the operator generated (opsolete runner-token -from-stdin): the runner's, replacing its old one.
INSERT INTO runners (name, pool, token_hash) VALUES (sqlc.arg(name)::text, sqlc.arg(pool)::text, sqlc.arg(token_hash)::bytea)
ON CONFLICT (name) DO UPDATE SET pool = EXCLUDED.pool, token_hash = EXCLUDED.token_hash;

-- name: RunnerByToken :one
SELECT id, name, pool, last_seen_at FROM runners WHERE token_hash = sqlc.arg(token_hash)::bytea;

-- name: TouchRunner :exec
UPDATE runners SET last_seen_at = now() WHERE id = sqlc.arg(id)::uuid;

-- name: Runners :many
SELECT id, name, pool, created_at, last_seen_at FROM runners ORDER BY pool, name;

-- name: DeleteRunner :one
DELETE FROM runners WHERE id = sqlc.arg(id)::uuid RETURNING name;

-- name: RunnerPools :many
-- The pools runners or projects name, for the project's pool picker.
SELECT pool FROM runners UNION SELECT runner_pool FROM projects UNION SELECT 'default' ORDER BY 1;
