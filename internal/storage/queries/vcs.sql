-- VCS connections and the registry modules published from repositories.

-- name: ResealVCSToken :execrows
UPDATE vcs_connections SET token = sqlc.arg(sealed)::bytea WHERE id = sqlc.arg(id)::uuid AND token = sqlc.arg(old)::bytea;

-- name: CreateVCSConnection :one
INSERT INTO vcs_connections (name, provider, base_url, token)
VALUES (sqlc.arg(name)::text, sqlc.arg(provider)::text, sqlc.arg(base_url)::text, sqlc.arg(token)::bytea) RETURNING id;

-- name: VCSConnections :many
SELECT * FROM vcs_connection_list ORDER BY name;

-- name: GetVCSConnection :one
SELECT * FROM vcs_connection_list WHERE id = sqlc.arg(id)::uuid;

-- name: SetVCSScope :exec
UPDATE vcs_connections SET workspace_scope = sqlc.arg(workspace_scope)::text WHERE id = sqlc.arg(id)::uuid;

-- name: DeleteVCSConnection :exec
DELETE FROM vcs_connections WHERE id = sqlc.arg(id)::uuid;

-- name: LinkVCSModule :exec
INSERT INTO vcs_modules (namespace, name, system, connection_id, repo, dir)
VALUES (sqlc.arg(namespace)::text, sqlc.arg(name)::text, sqlc.arg(system)::text, sqlc.arg(connection_id)::uuid, sqlc.arg(repo)::text, sqlc.arg(dir)::text);

-- name: VCSModules :many
SELECT m.namespace, m.name, m.system, m.connection_id, c.name AS connection, c.provider, m.repo, m.dir, m.synced_at, m.sync_error
FROM vcs_modules m JOIN vcs_connections c ON c.id = m.connection_id ORDER BY m.namespace, m.name, m.system;

-- name: GetVCSModule :one
SELECT m.namespace, m.name, m.system, m.connection_id, c.name AS connection, c.provider, m.repo, m.dir, m.synced_at, m.sync_error
FROM vcs_modules m JOIN vcs_connections c ON c.id = m.connection_id
WHERE m.namespace = sqlc.arg(namespace)::text AND m.name = sqlc.arg(name)::text AND m.system = sqlc.arg(system)::text;

-- name: UnlinkVCSModule :execrows
DELETE FROM vcs_modules WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text;

-- name: SetVCSSync :exec
UPDATE vcs_modules SET synced_at = now(), sync_error = sqlc.arg(sync_error)::text
WHERE namespace = sqlc.arg(namespace)::text AND name = sqlc.arg(name)::text AND system = sqlc.arg(system)::text;
