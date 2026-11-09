-- Users: keyed by subject (OIDC issuer + sub); the dev user has none and is keyed by email.

-- name: UpsertUserByEmail :one
INSERT INTO users (email, role, groups) VALUES (sqlc.arg(email)::text, sqlc.arg(role)::text, sqlc.arg(groups)::text[])
ON CONFLICT (email) DO UPDATE SET role = EXCLUDED.role, groups = EXCLUDED.groups RETURNING id;

-- name: UpdateUserBySubject :one
-- the subject's user, or one without a subject yet with this email (bound to it now)
UPDATE users SET subject = sqlc.arg(subject)::text, email = sqlc.arg(email)::text, role = sqlc.arg(role)::text, groups = sqlc.arg(groups)::text[]
WHERE subject = sqlc.arg(subject)::text
    OR (subject IS NULL AND email = sqlc.arg(email)::text AND NOT EXISTS (SELECT 1 FROM users x WHERE x.subject = sqlc.arg(subject)::text))
RETURNING id;

-- name: UpsertUserBySubject :one
-- a concurrent first login of the same subject wins the insert
INSERT INTO users (subject, email, role, groups) VALUES (sqlc.arg(subject)::text, sqlc.arg(email)::text, sqlc.arg(role)::text, sqlc.arg(groups)::text[])
ON CONFLICT (subject) DO UPDATE SET email = EXCLUDED.email, role = EXCLUDED.role, groups = EXCLUDED.groups
RETURNING id;

-- name: UserGroups :one
SELECT array_to_json(groups)::json FROM users WHERE id = sqlc.arg(id)::uuid;

-- name: KnownGroups :many
SELECT DISTINCT unnest(groups)::text AS g FROM users ORDER BY g;
