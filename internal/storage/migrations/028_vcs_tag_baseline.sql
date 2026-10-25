-- Commit of the newest matching tag when tag-based triggers were set: it is what already ran (or
-- an old release), so only tags pushed later queue runs, as in HCP Terraform.
ALTER TABLE workspaces ADD COLUMN vcs_tag_baseline text NOT NULL DEFAULT '';
