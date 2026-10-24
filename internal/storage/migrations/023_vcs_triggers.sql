-- VCS-driven runs (as in HCP Terraform), found by polling (see vcs.TriggerRuns):
-- vcs_trigger: a new commit on the linked branch queues an apply run of that commit.
-- vcs_pull_requests: open pull requests into the branch get a speculative plan ('plan' runs: never applied).
-- Off for workspaces linked before: their runs were only ever started by hand.
ALTER TABLE workspaces ADD COLUMN vcs_trigger boolean NOT NULL DEFAULT false,
    ADD COLUMN vcs_pull_requests boolean NOT NULL DEFAULT false;
ALTER TABLE runs DROP CONSTRAINT runs_action_check,
    ADD CONSTRAINT runs_action_check CHECK (action IN ('apply', 'destroy', 'refresh', 'assess', 'plan')),
    ADD COLUMN vcs_pull_request integer; -- the pull request a speculative plan was made for
CREATE INDEX runs_vcs_commit_idx ON runs (workspace_id, vcs_commit) WHERE vcs_commit IS NOT NULL;
