-- Generated documentation: the full terraform-config-inspect module plus the archive's README.md.
-- Rows uploaded before this migration have no docs; upload a new version to get them.
ALTER TABLE modules
    ADD COLUMN docs   jsonb,
    ADD COLUMN readme text NOT NULL DEFAULT '';
