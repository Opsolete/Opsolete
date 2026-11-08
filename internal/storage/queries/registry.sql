-- Registry modules and API tokens.

-- name: CreateModule :exec
INSERT INTO modules (namespace, name, system, version, archive, variables, docs, readme)
VALUES (sqlc.arg(namespace)::text, sqlc.arg(name)::text, sqlc.arg(system)::text, sqlc.arg(version)::text, sqlc.arg(archive)::bytea,
    sqlc.arg(variables)::jsonb, sqlc.narg(docs)::jsonb, sqlc.arg(readme)::text);

-- name: ListModules :many
-- newest upload first
SELECT namespace, name, system, version, variables, uploaded_at FROM modules ORDER BY namespace, name, system, uploaded_at DESC;

-- name: ModuleVersions :many
SELECT version FROM modules WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text;

-- name: GetModule :one
SELECT namespace, name, system, version, variables, uploaded_at FROM modules
WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text AND version = sqlc.arg(version)::text;

-- name: GetModuleDocs :one
SELECT docs, readme FROM modules
WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text AND version = sqlc.arg(version)::text;

-- name: ModuleArchive :one
SELECT archive FROM modules
WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text AND version = sqlc.arg(version)::text;

-- name: DeleteModuleVersions :execrows
DELETE FROM modules WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text;

-- name: CreateToken :exec
INSERT INTO api_tokens (name, token_hash, scope) VALUES (sqlc.arg(name)::text, sqlc.arg(token_hash)::bytea, sqlc.arg(scope)::text);

-- name: ValidToken :one
-- an unexpired user token also passes as a registry token
SELECT EXISTS (SELECT 1 FROM api_tokens WHERE token_hash = sqlc.arg(token_hash)::bytea
    AND (scope = sqlc.arg(scope)::text OR (sqlc.arg(scope)::text = 'registry' AND scope = 'user' AND expires_at > now())));

-- name: CreateUserToken :exec
INSERT INTO api_tokens (name, token_hash, scope, user_id, expires_at)
VALUES (sqlc.arg(name)::text, sqlc.arg(token_hash)::bytea, 'user', sqlc.arg(user_id)::uuid, now() + make_interval(secs => sqlc.arg(ttl_secs)::float8));

-- name: TokenUser :one
SELECT u.id, u.email, u.role, u.groups FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = sqlc.arg(token_hash)::bytea AND t.scope = 'user' AND t.expires_at > now();

-- name: UserTokens :many
-- unexpired, newest first
SELECT id, name, created_at, expires_at::timestamptz AS expires_at FROM api_tokens
WHERE scope = 'user' AND user_id = sqlc.arg(user_id)::uuid AND expires_at > now() ORDER BY created_at DESC;

-- name: RevokeUserToken :exec
-- other users' token ids are ignored
DELETE FROM api_tokens WHERE scope = 'user' AND user_id = sqlc.arg(user_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: RevokeUserTokens :exec
DELETE FROM api_tokens WHERE scope = 'user' AND user_id = sqlc.arg(user_id)::uuid;
