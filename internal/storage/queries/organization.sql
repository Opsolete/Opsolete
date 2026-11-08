-- The one row of instance settings.

-- name: Currency :one
SELECT currency FROM organization;

-- name: SetCurrency :exec
UPDATE organization SET currency = sqlc.arg(currency)::text;

-- name: ModuleUploads :one
SELECT module_uploads FROM organization;

-- name: SetModuleUploads :exec
UPDATE organization SET module_uploads = sqlc.arg(module_uploads)::boolean;

-- name: SigningKey :one
-- concurrent first starts agree: the first key set wins
UPDATE organization SET signing_key = COALESCE(signing_key, sqlc.arg(key)::bytea) RETURNING signing_key::bytea;
