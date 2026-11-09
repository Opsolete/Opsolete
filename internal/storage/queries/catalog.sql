-- The template catalog: the Git repository templates come from.
-- A sync's writes only apply to the row it started from (commit, connection, repo, branch, dir):
-- a sync that was overtaken or reconfigured meanwhile writes nothing.

-- name: GetTemplateCatalog :one
SELECT connection_id, repo, branch, dir, mode, commit_sha, files, synced_at, sync_error FROM template_catalog;

-- name: TemplateCatalogVersion :one
-- what a replica compares with its loaded catalog, without reading the files
SELECT mode, commit_sha FROM template_catalog;

-- name: SetTemplateCatalog :exec
-- commit_sha '': the next sync loads it afresh (another folder may have the same commit)
INSERT INTO template_catalog (connection_id, repo, branch, dir, mode)
VALUES (sqlc.arg(connection_id)::uuid, sqlc.arg(repo)::text, sqlc.arg(branch)::text, sqlc.arg(dir)::text, sqlc.arg(mode)::text)
ON CONFLICT (id) DO UPDATE SET connection_id = EXCLUDED.connection_id, repo = EXCLUDED.repo, branch = EXCLUDED.branch,
    dir = EXCLUDED.dir, mode = EXCLUDED.mode, commit_sha = '';

-- name: CatalogSynced :execrows
UPDATE template_catalog SET sync_error = '', commit_sha = sqlc.arg(commit)::text, files = sqlc.arg(files)::jsonb, synced_at = now()
WHERE commit_sha = sqlc.arg(prev_commit)::text AND connection_id = sqlc.arg(connection_id)::uuid AND repo = sqlc.arg(repo)::text
    AND branch = sqlc.arg(branch)::text AND dir = sqlc.arg(dir)::text;

-- name: CatalogSyncFailed :execrows
-- keeps the last good files and synced_at (the last success)
UPDATE template_catalog SET sync_error = sqlc.arg(sync_error)::text
WHERE commit_sha = sqlc.arg(prev_commit)::text AND connection_id = sqlc.arg(connection_id)::uuid AND repo = sqlc.arg(repo)::text
    AND branch = sqlc.arg(branch)::text AND dir = sqlc.arg(dir)::text;

-- name: CatalogChecked :execrows
-- the branch is still at the commit: clears any earlier error
UPDATE template_catalog SET sync_error = '', synced_at = now()
WHERE commit_sha = sqlc.arg(prev_commit)::text AND connection_id = sqlc.arg(connection_id)::uuid AND repo = sqlc.arg(repo)::text
    AND branch = sqlc.arg(branch)::text AND dir = sqlc.arg(dir)::text;

-- name: DeleteTemplateCatalog :exec
DELETE FROM template_catalog;
