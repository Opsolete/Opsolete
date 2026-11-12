-- Promotions (011_run_state.sql, storage/promote.go).

-- name: InsertPromotion :exec
INSERT INTO promotions (target_workspace_id, source, cv_id, created_by, actor, destroy_run_id, apply_run_id)
VALUES (sqlc.arg(target_workspace_id)::uuid, sqlc.arg(source)::text, sqlc.arg(cv_id)::uuid, sqlc.narg(created_by)::uuid,
    sqlc.arg(actor)::text, sqlc.narg(destroy_run_id)::uuid, sqlc.narg(apply_run_id)::uuid);

-- name: PromotionAfter :one
-- The promotion whose destroy run is run_id and whose apply run isn't queued yet.
SELECT id, source, cv_id, created_by, actor FROM promotions
WHERE destroy_run_id = sqlc.arg(run_id)::uuid AND apply_run_id IS NULL FOR UPDATE;

-- name: SetPromotionApplyRun :exec
UPDATE promotions SET apply_run_id = sqlc.arg(apply_run_id)::uuid WHERE id = sqlc.arg(id)::uuid;
