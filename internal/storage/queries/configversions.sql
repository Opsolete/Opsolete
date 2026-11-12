-- Configuration versions (010_configuration_versions.sql, storage/configversions.go).

-- name: InsertConfigVersion :one
INSERT INTO configuration_versions (workspace_id, parent_id, source, created_by)
VALUES (sqlc.arg(workspace_id)::uuid, sqlc.narg(parent_id)::uuid, sqlc.arg(source)::text, sqlc.narg(created_by)::uuid)
RETURNING id;

-- name: InsertConfigModule :exec
INSERT INTO configuration_modules (cv_id, name, template_id, module_source, module_ref, inputs)
VALUES (sqlc.arg(cv_id)::uuid, sqlc.arg(name)::text, sqlc.arg(template_id)::text, sqlc.arg(module_source)::text,
    sqlc.arg(module_ref)::text, sqlc.arg(inputs)::jsonb);

-- name: ConfigModules :many
SELECT name, template_id, module_source, module_ref, inputs FROM configuration_modules WHERE cv_id = sqlc.arg(cv_id)::uuid ORDER BY name;

-- name: CurrentConfig :one
-- The row lock keeps runs from being queued meanwhile (their FK check waits) until the transaction ends.
SELECT current_cv_id FROM workspaces WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: SetCurrentConfig :exec
UPDATE workspaces SET current_cv_id = sqlc.arg(cv_id)::uuid WHERE id = sqlc.arg(id)::uuid;

-- name: RunConfig :one
-- A run's version and its parent; changed: the run is the first to plan it (it made the change, the runs after
-- it re-plan it); applied: its apply started (the workspace took the version).
SELECT r.cv_id, cv.parent_id, run_active(r.status)::boolean AS active,
    (cv.id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM runs x WHERE x.cv_id = r.cv_id AND x.created_at < r.created_at))::boolean AS changed,
    EXISTS (SELECT 1 FROM run_status_history h WHERE h.run_id = r.id AND h.status IN ('applying', 'no_changes'))::boolean AS applied
FROM runs r LEFT JOIN configuration_versions cv ON cv.id = r.cv_id WHERE r.id = sqlc.arg(id)::uuid;

-- name: AllConfigInputs :many
SELECT cv.workspace_id, m.cv_id, m.name, m.inputs FROM configuration_modules m JOIN configuration_versions cv ON cv.id = m.cv_id;

-- name: ResealConfigInputs :execrows
UPDATE configuration_modules SET inputs = sqlc.arg(inputs)::jsonb
WHERE cv_id = sqlc.arg(cv_id)::uuid AND name = sqlc.arg(name)::text AND inputs = sqlc.arg(old)::jsonb;
