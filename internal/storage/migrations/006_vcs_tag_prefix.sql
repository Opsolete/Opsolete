-- tag_prefix: only tags starting with it publish, the rest of the tag being the version: a repository with
-- several modules, each released on its own (release-please: data-disk-v1.0.0 with prefix "data-disk-").
-- '' takes every semver tag.
ALTER TABLE vcs_modules ADD COLUMN tag_prefix text NOT NULL DEFAULT '';
