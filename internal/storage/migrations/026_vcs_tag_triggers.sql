-- Tag-based VCS triggers (as in HCP Terraform): a new tag matching vcs_tags_regex queues an apply run
-- of the tag's commit, instead of new commits on vcs_branch. '' is branch-based.
ALTER TABLE workspaces ADD COLUMN vcs_tags_regex text NOT NULL DEFAULT '';
