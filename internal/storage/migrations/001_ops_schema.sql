-- OPS schema: the migrations up to 0.11 squashed into one. Later versions add numbered migrations after this
-- file; databases from before the squash are not upgraded (Migrate refuses them): this starts from a fresh one.
--
-- Rules that live in Go, not here: the run state machine (storage/runs.go) and every side effect
-- of a run ending. The schema only keeps the invariants a bug must never break: one active run per
-- workspace, valid statuses and levels, an append-only audit log.

-- People and access ---------------------------------------------------------------------------

-- A user is keyed by subject (OIDC issuer + sub); the dev user has none. email, role and groups
-- are refreshed at every login. role is what the login decided; requests check it against the
-- config again (auth.Role).
CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject    text UNIQUE,
    email      text NOT NULL UNIQUE,
    role       text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    groups     text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- api_tokens: registry tokens (opsolete token <name>, no user, no expiry), user tokens (terraform login, a user and
-- an expiry) and service tokens: automation (the Terraform provider, scripts) acts with admin rights as the token
-- itself, not as a person; no user, an expiry only if one was given, names unique so an audit entry token:<name>
-- names one token. Only the SHA-256 of a token is stored.
CREATE TABLE api_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    scope      text NOT NULL CHECK (scope IN ('registry', 'user', 'service')),
    user_id    uuid REFERENCES users ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((scope = 'user') = (user_id IS NOT NULL AND expires_at IS NOT NULL))
);
CREATE INDEX api_tokens_user_idx ON api_tokens (user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX api_tokens_service_name_idx ON api_tokens (name) WHERE scope = 'service';

-- sessions: browser logins; the cookie holds the token, only its SHA-256 is stored. admin_email and
-- admin_groups are what can make the session admin (re-checked against ADMIN_EMAILS/ADMIN_GROUPS per
-- request); user_agent and ip (truncated) are shown on the account page.
CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   bytea NOT NULL UNIQUE,
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    admin_email  text NOT NULL DEFAULT '',
    admin_groups text[] NOT NULL DEFAULT '{}',
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- runner_pool: only runners of this pool get its workspaces' jobs (runners.pool).
CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    runner_pool text NOT NULL DEFAULT 'default'
);

-- organization is the one row of instance settings.
CREATE TABLE organization (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    default_project_id uuid NOT NULL REFERENCES projects, -- also keeps the default project from being deleted
    currency           text NOT NULL DEFAULT 'USD' CHECK (currency IN ('USD', 'EUR')), -- of cost estimates
    module_uploads     boolean NOT NULL DEFAULT false, -- admins may upload modules to the registry
    signing_key        bytea -- random; set at first start (Store.SigningKey), mixed with SECRET_KEY before use
);
WITH p AS (INSERT INTO projects (name) VALUES ('default') RETURNING id)
INSERT INTO organization (default_project_id) SELECT id FROM p;

CREATE TABLE project_access (
    project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    group_name text NOT NULL,
    level      text NOT NULL CHECK (level IN ('read', 'operate')),
    PRIMARY KEY (project_id, group_name)
);

-- runners: one row per runner, with its own token (only the SHA-256 is stored). Deleting a row revokes the runner.
CREATE TABLE runners (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL UNIQUE,
    pool         text NOT NULL DEFAULT 'default',
    token_hash   bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);

-- VCS connections ------------------------------------------------------------------------------

-- token is sealed (AES-GCM, bound to provider, base_url and name). workspace_scope: who may link
-- workspaces to it: nobody (''), every workspace ('all'), or the selected workspaces and projects.
CREATE TABLE vcs_connections (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL UNIQUE,
    provider        text NOT NULL CHECK (provider IN ('github', 'azure-devops')),
    base_url        text NOT NULL,
    token           bytea NOT NULL,
    workspace_scope text NOT NULL DEFAULT '' CHECK (workspace_scope IN ('', 'all', 'selected')),
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE vcs_connection_projects (
    connection_id uuid NOT NULL REFERENCES vcs_connections ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    PRIMARY KEY (connection_id, project_id)
);
CREATE INDEX vcs_connection_projects_project_idx ON vcs_connection_projects (project_id);

-- Workspaces -----------------------------------------------------------------------------------

-- A workspace holds one Terraform state. Its configuration is its added templates (a configuration version,
-- current_cv_id; workspace_templates) next to a VCS branch's root configuration (workspace_vcs), or nothing.
-- state_lock is Terraform's LockInfo plus OPSRun, the run holding it (absent for CLI and UI locks).
CREATE TABLE workspaces (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                text NOT NULL UNIQUE,
    project_id          uuid NOT NULL REFERENCES projects ON DELETE RESTRICT, -- only empty projects can be deleted
    created_by          uuid REFERENCES users,
    auto_apply          boolean NOT NULL DEFAULT false,
    drift_detection     boolean NOT NULL DEFAULT false,
    global_remote_state boolean NOT NULL DEFAULT false, -- every workspace may read its state; else remote_state_consumers
    execution_mode      text NOT NULL DEFAULT 'remote' CHECK (execution_mode IN ('remote', 'local')),
    state_lock          jsonb,
    state_locked_at     timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK ((state_lock IS NULL) = (state_locked_at IS NULL))
);
CREATE INDEX workspaces_project_idx ON workspaces (project_id);
CREATE INDEX workspaces_created_by_idx ON workspaces (created_by) WHERE created_by IS NOT NULL;

-- workspace_vcs: a row iff the workspace is linked to a repository branch, its root configuration.
-- trigger: new commits queue apply runs; pull_requests: open pull requests get speculative plans;
-- tags_regex <> '': tag-based instead (new matching tags queue runs, pull requests are ignored), and
-- tag_baseline is the newest matching tag's commit when that was set, never queued.
CREATE TABLE workspace_vcs (
    workspace_id  uuid PRIMARY KEY REFERENCES workspaces ON DELETE CASCADE,
    connection_id uuid NOT NULL REFERENCES vcs_connections ON DELETE CASCADE,
    repo          text NOT NULL,
    branch        text NOT NULL,
    trigger       boolean NOT NULL DEFAULT false,
    pull_requests boolean NOT NULL DEFAULT false,
    tags_regex    text NOT NULL DEFAULT '',
    tag_baseline  text NOT NULL DEFAULT '',
    CHECK (tags_regex = '' OR (trigger AND NOT pull_requests))
);
CREATE INDEX workspace_vcs_connection_idx ON workspace_vcs (connection_id);

-- vcs_connection_workspaces: the workspaces a 'selected' connection allows besides its projects'.
CREATE TABLE vcs_connection_workspaces (
    connection_id uuid NOT NULL REFERENCES vcs_connections ON DELETE CASCADE,
    workspace_id  uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    PRIMARY KEY (connection_id, workspace_id)
);
CREATE INDEX vcs_connection_workspaces_workspace_idx ON vcs_connection_workspaces (workspace_id);

CREATE TABLE workspace_access (
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    group_name   text NOT NULL,
    level        text NOT NULL CHECK (level IN ('read', 'operate')),
    PRIMARY KEY (workspace_id, group_name)
);

CREATE TABLE remote_state_consumers (
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    consumer_id  uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    PRIMARY KEY (workspace_id, consumer_id),
    CHECK (workspace_id <> consumer_id)
);
CREATE INDEX remote_state_consumers_consumer_idx ON remote_state_consumers (consumer_id);

-- access_level is what a user may do in a workspace without being an admin: 'operate' as its
-- creator, else the highest of the user's group grants on the workspace and on its project; NULL
-- for nothing. The one definition of access: every query and Store.Access use it.
CREATE FUNCTION access_level(ws uuid, viewer uuid) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN w.created_by = viewer THEN 'operate'
        WHEN bool_or(g.level = 'operate') THEN 'operate'
        WHEN count(g.level) > 0 THEN 'read'
    END
    FROM workspaces w
    LEFT JOIN users u ON u.id = viewer
    LEFT JOIN LATERAL (
        SELECT level FROM workspace_access a WHERE a.workspace_id = w.id AND a.group_name = ANY (u.groups)
        UNION ALL
        SELECT level FROM project_access a WHERE a.project_id = w.project_id AND a.group_name = ANY (u.groups)
    ) g ON true
    WHERE w.id = ws
    GROUP BY w.id, w.created_by
$$;

-- Variables ------------------------------------------------------------------------------------

CREATE TABLE variable_sets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    global      boolean NOT NULL DEFAULT false, -- applies to every workspace without a VCS link
    created_at  timestamptz NOT NULL DEFAULT now(),
    priority    boolean NOT NULL DEFAULT false -- its variables win over a workspace's own and other sets' (values an admin enforces)
);

CREATE TABLE variable_set_workspaces (
    set_id       uuid NOT NULL REFERENCES variable_sets ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    PRIMARY KEY (set_id, workspace_id)
);
CREATE INDEX variable_set_workspaces_workspace_idx ON variable_set_workspaces (workspace_id);

CREATE TABLE variable_set_projects (
    set_id     uuid NOT NULL REFERENCES variable_sets ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    PRIMARY KEY (set_id, project_id)
);
CREATE INDEX variable_set_projects_project_idx ON variable_set_projects (project_id);

-- variables belong to a workspace or a set. value is sealed (AES-GCM, bound to owner, category,
-- key and sensitivity). hcl: a Terraform variable whose value is an HCL expression (a list, map, object,
-- number or bool); otherwise the value is a string.
CREATE TABLE variables (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid REFERENCES workspaces ON DELETE CASCADE,
    set_id       uuid REFERENCES variable_sets ON DELETE CASCADE,
    category     text NOT NULL CHECK (category IN ('env', 'terraform')),
    key          text NOT NULL,
    value        bytea NOT NULL,
    sensitive    boolean NOT NULL DEFAULT false,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((workspace_id IS NULL) <> (set_id IS NULL)),
    UNIQUE (workspace_id, category, key),
    UNIQUE (set_id, category, key),
    hcl          boolean NOT NULL DEFAULT false,
    CONSTRAINT variables_hcl_terraform CHECK (NOT hcl OR category = 'terraform')
);

-- variable_set_rank is how a variable set applies to a workspace's runs: 1 attached to it, 2
-- attached to its project, 3 global, NULL not at all; a workspace's own variables come before all
-- (rank 0). Project and global sets don't reach a workspace linked to a repository: that code is
-- whatever a user chose to link, so only an admin attaching a set may give it credentials (anyone
-- who operates the workspace may relink it, and a project set, unlike an attached one, isn't
-- detached when the link changes). The one definition: RunVariables and WorkspaceVariableSets use it.
CREATE FUNCTION variable_set_rank(ws uuid, vs uuid) RETURNS integer LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN EXISTS (SELECT 1 FROM variable_set_workspaces a WHERE a.set_id = vs AND a.workspace_id = ws) THEN 1
        WHEN EXISTS (SELECT 1 FROM workspace_vcs v WHERE v.workspace_id = ws) THEN NULL
        WHEN EXISTS (SELECT 1 FROM variable_set_projects a JOIN workspaces w ON w.project_id = a.project_id
            WHERE a.set_id = vs AND w.id = ws) THEN 2
        WHEN (SELECT s.global FROM variable_sets s WHERE s.id = vs) THEN 3
    END
$$;

-- Templates ------------------------------------------------------------------------------------

-- template_catalog: the one Git repository templates come from instead of TEMPLATES_DIR. files is
-- the last loaded commit's template files (path -> JSON); mode: how the builder saves ('off', a
-- commit, or a pull request).
CREATE TABLE template_catalog (
    id            boolean PRIMARY KEY DEFAULT true CHECK (id),
    connection_id uuid NOT NULL REFERENCES vcs_connections,
    repo          text NOT NULL,
    branch        text NOT NULL,
    dir           text NOT NULL DEFAULT '',
    mode          text NOT NULL CHECK (mode IN ('off', 'commit', 'pr')),
    commit_sha    text NOT NULL DEFAULT '',
    files         jsonb NOT NULL DEFAULT '{}',
    synced_at     timestamptz,
    sync_error    text NOT NULL DEFAULT ''
);

-- template_rollouts: upgrading every workspace that uses a template (in one project, or all) to a
-- module version, batch_size runs at a time (0: all at once). At most one open rollout per template.
CREATE TABLE template_rollouts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    project_id    uuid REFERENCES projects ON DELETE CASCADE,
    batch_size    integer NOT NULL DEFAULT 0 CHECK (batch_size >= 0),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'done', 'stopped')),
    message       text NOT NULL DEFAULT '',
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    resumed_at    timestamptz NOT NULL DEFAULT now(),
    ticked_at     timestamptz
);
CREATE UNIQUE INDEX template_rollouts_open_idx ON template_rollouts (template_id) WHERE status IN ('active', 'paused');

-- environment_values: per-environment template inputs (template field per_environment): a project's value for an
-- input of a template, applied to every workspace in the project at each run, over the workspace's own. value is
-- the input's canonical form string ("7", "true"), as templates hold defaults. keep_own: a workspace with its own
-- value for the input (saved before the project set one, or an imported resource's) keeps it; else all take it.
CREATE TABLE environment_values (
    project_id  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    template_id text NOT NULL,
    input_name  text NOT NULL,
    value       text NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid REFERENCES users ON DELETE SET NULL,
    keep_own    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (project_id, template_id, input_name)
);

-- Configuration versions (docs/configuration-versions.md) ------------------------------------

-- A workspace's templates are an immutable version; a run plans one (runs.cv_id) and the workspace points at the
-- one whose apply started (workspaces.current_cv_id).
CREATE TABLE configuration_versions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    parent_id    uuid REFERENCES configuration_versions ON DELETE SET NULL, -- the current version it was made from
    source       text NOT NULL CHECK (source IN ('form', 'promote', 'rollout', 'import', 'remove', 'upgrade')),
    created_by   uuid REFERENCES users,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX configuration_versions_workspace_idx ON configuration_versions (workspace_id);

-- configuration_modules: a version's template modules, each the module block module.<name>, pinned to a module
-- version. inputs: the form values; sensitive ones are {"sealed": "<base64>"} (rotate-key re-seals them here).
-- follows: the per-environment inputs that take the project's value even when it lets workspaces keep their own
-- (keep_own), decided when the form was saved (app.EnvironmentMap.Locked). importing: the module imports what
-- exists (the template's import switch, app.ImportInput, added to its inputs for the runner only).
CREATE TABLE configuration_modules (
    cv_id         uuid NOT NULL REFERENCES configuration_versions ON DELETE CASCADE,
    name          text NOT NULL,
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    inputs        jsonb NOT NULL,
    follows       text[] NOT NULL DEFAULT '{}',
    importing     boolean NOT NULL DEFAULT false,
    PRIMARY KEY (cv_id, name)
);
CREATE INDEX configuration_modules_template_idx ON configuration_modules (template_id);

ALTER TABLE workspaces ADD COLUMN current_cv_id uuid REFERENCES configuration_versions ON DELETE SET NULL;

-- Runs -----------------------------------------------------------------------------------------

-- A run is one plan, then (confirmed or auto-applied) one apply, of a workspace's configuration version (cv_id).
-- The statuses and every allowed transition are in storage/runs.go; a run is active from plan_queued to applying
-- (run_active). A run started while a blocking one is active waits as pending; when that one ends, the oldest
-- pending run is queued (storage.promotePending).
--   action: apply | destroy | refresh (refresh-only) | assess (drift check, never applied)
--           | plan (speculative plan of a pull request, never applied)
--   lease_id/lease_until: the claiming runner's fencing token, while planning or applying
--   lease_acked_at: when the runner acknowledged the claim (storage.AckJob); NULL until then, and an
--           unacknowledged claim goes back to the queue (storage.UnclaimJobs)
--   remove_templates: a destroy run that removes those added templates (-target module.<name> each)
--   found: what a plan that imports found, per template module, the inputs and their real values
--           (runnerapi.FinishRequest.Found), for the form's "Use found values"
--   confirm_reason: why the run's plan waits for a human (storage.confirmReason); set when it does
--   plan_diff: the plan's attribute changes per resource (runnerapi.ResourceDiff, values masked), apart
--           from plan (counts and addresses), which every run list reads
--   log_removed_at: retention (app/retention.go) deleted its run_logs then; updated_at is when it ended
-- run_active: the run holds its workspace (storage.Ended is its negation; a test keeps them equal).
-- run_working: a runner holds its lease. Every query and index that asks either uses these.
-- run_blocking: an active run that holds the workspace, one per workspace (runs_one_active_idx). Drift checks and
-- pull requests' speculative plans run beside it (they read the state without locking it and run the current
-- configuration): one active drift check per workspace, one active plan per pull request. Self-contained
-- (run_active's statuses written out): PostgreSQL inlines index predicates with a restricted search_path, where
-- run_active can't be found by name.
CREATE FUNCTION run_active(status text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
$$;
CREATE FUNCTION run_working(status text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT status IN ('planning', 'applying')
$$;
CREATE FUNCTION run_blocking(status text, action text, pull_request integer) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying')
        AND action <> 'assess' AND pull_request IS NULL
$$;

CREATE TABLE runs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id     uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    action           text NOT NULL CHECK (action IN ('apply', 'destroy', 'refresh', 'assess', 'plan')),
    status           text NOT NULL CHECK (status IN ('pending', 'plan_queued', 'planning', 'needs_confirmation',
                         'apply_queued', 'applying', 'applied', 'no_changes', 'drifted', 'planned', 'discarded', 'errored',
                         'canceled')),
    replace          jsonb, -- resource addresses the plan replaces (-replace)
    plan             jsonb, -- runnerapi.Plan: counts and addresses, never values
    outputs          jsonb, -- non-sensitive outputs after the apply
    runner           text,
    lease_id         text,
    lease_until      timestamptz,
    lease_acked_at   timestamptz,
    cancel_requested boolean NOT NULL DEFAULT false,
    log_bytes        bigint NOT NULL DEFAULT 0,
    log_removed_at   timestamptz,
    vcs_commit       text,    -- pinned by the plan (or when queued for a commit)
    vcs_pull_request integer, -- speculative plans
    rollout_id       uuid REFERENCES template_rollouts ON DELETE SET NULL,
    created_by       uuid REFERENCES users,
    confirmed_by     uuid REFERENCES users,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    remove_templates text[],
    found            jsonb,
    cv_id            uuid REFERENCES configuration_versions ON DELETE SET NULL,
    confirm_reason   text,
    plan_diff        jsonb,
    CHECK ((lease_id IS NULL) = (lease_until IS NULL)),
    CHECK (lease_id IS NULL OR run_working(status)),
    CHECK (lease_acked_at IS NULL OR lease_id IS NOT NULL),
    CHECK (remove_templates IS NULL OR action = 'destroy'),
    CHECK (vcs_pull_request IS NULL OR action = 'plan')
);
CREATE UNIQUE INDEX runs_one_active_idx ON runs (workspace_id) WHERE run_blocking(status, action, vcs_pull_request);
CREATE UNIQUE INDEX runs_one_assess_idx ON runs (workspace_id)
    WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying') AND action = 'assess';
CREATE UNIQUE INDEX runs_one_pull_request_idx ON runs (workspace_id, vcs_pull_request)
    WHERE status IN ('plan_queued', 'planning', 'needs_confirmation', 'apply_queued', 'applying') AND vcs_pull_request IS NOT NULL;
CREATE INDEX runs_pending_idx ON runs (workspace_id, created_at) WHERE status = 'pending';
CREATE INDEX runs_queue_idx ON runs (updated_at) WHERE status IN ('plan_queued', 'apply_queued');
CREATE INDEX runs_leases_idx ON runs (lease_until) WHERE lease_id IS NOT NULL;
CREATE INDEX runs_workspace_idx ON runs (workspace_id, created_at DESC);
CREATE INDEX runs_applied_idx ON runs (workspace_id, updated_at) WHERE status = 'applied';
CREATE INDEX runs_assess_idx ON runs (workspace_id, created_at DESC) WHERE action = 'assess';
CREATE INDEX runs_vcs_commit_idx ON runs (workspace_id, vcs_commit) WHERE vcs_commit IS NOT NULL;
CREATE INDEX runs_rollout_idx ON runs (rollout_id) WHERE rollout_id IS NOT NULL;
CREATE INDEX runs_created_by_idx ON runs (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX runs_confirmed_by_idx ON runs (confirmed_by) WHERE confirmed_by IS NOT NULL;
-- retention (queries/runs.sql Prune*): ended runs whose log is still there; drift checks and speculative plans
CREATE INDEX runs_log_retention_idx ON runs (updated_at) WHERE log_removed_at IS NULL AND NOT run_active(status);
CREATE INDEX runs_speculative_idx ON runs (updated_at) WHERE action IN ('assess', 'plan');

CREATE TABLE run_status_history (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    status     text NOT NULL,
    message    text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_status_history_run_idx ON run_status_history (run_id, id);

CREATE TABLE run_logs (
    seq        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    chunk      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX run_logs_run_idx ON run_logs (run_id, seq);

-- promotions: a promotion's target templates are a configuration version (cv_id), applied by apply_run_id; one
-- that removes or replaces a deployed template destroys them first (destroy_run_id), and the destroy applied
-- queues the apply run (storage.continuePromotion).
CREATE TABLE promotions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target_workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    source              text NOT NULL,  -- the source workspace's name, for history and audit
    cv_id               uuid REFERENCES configuration_versions ON DELETE SET NULL,
    created_by          uuid REFERENCES users,
    actor               text NOT NULL DEFAULT '', -- created_by's email, for the chained apply run's audit entry
    destroy_run_id      uuid UNIQUE REFERENCES runs ON DELETE SET NULL,
    apply_run_id        uuid UNIQUE REFERENCES runs ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- State ----------------------------------------------------------------------------------------

-- state_versions: immutable; the file is in STATE_STORE at blob_key, sealed. indexed: resources
-- were recorded (state_version_resources); an unindexed current version counts as deployed.
CREATE TABLE state_versions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    serial      bigint NOT NULL,
    lineage     text NOT NULL,
    blob_key    text NOT NULL UNIQUE,
    md5         text NOT NULL,
    size        bigint NOT NULL,
    outputs     jsonb, -- non-sensitive outputs
    indexed     boolean NOT NULL DEFAULT false,
    rollback_of bigint REFERENCES state_versions ON DELETE SET NULL, -- a rollback's copy of that version
    run_id      uuid REFERENCES runs ON DELETE SET NULL, -- the run that wrote it; NULL for the CLI and rollbacks
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX state_versions_workspace_idx ON state_versions (workspace_id, id DESC);
CREATE INDEX state_versions_rollback_of_idx ON state_versions (rollback_of) WHERE rollback_of IS NOT NULL;
CREATE INDEX state_versions_run_idx ON state_versions (run_id) WHERE run_id IS NOT NULL;

-- state_version_resources: names and types only, never attribute values. resource_id: the resource's id attribute
-- (runnerapi.Resource.ID): an import of a managed resource is refused.
CREATE TABLE state_version_resources (
    state_version_id bigint NOT NULL REFERENCES state_versions ON DELETE CASCADE,
    address          text NOT NULL,
    type             text NOT NULL,
    name             text NOT NULL,
    provider         text NOT NULL,
    module           text NOT NULL DEFAULT '',
    resource_id      text,
    PRIMARY KEY (state_version_id, address)
);
CREATE INDEX state_version_resources_id_idx ON state_version_resources (lower(resource_id)) WHERE resource_id IS NOT NULL;

-- Registry -------------------------------------------------------------------------------------

-- modules: published module versions. variables: what terraform-config-inspect found (for forms);
-- docs: inputs, outputs and resources for the module page.
CREATE TABLE modules (
    namespace   text NOT NULL,
    name        text NOT NULL,
    system      text NOT NULL,
    version     text NOT NULL,
    archive     bytea NOT NULL,
    variables   jsonb NOT NULL,
    docs        jsonb,
    readme      text NOT NULL DEFAULT '',
    uploaded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, name, system, version)
);

-- vcs_modules: a registry module published from a repository's semver tags (dir: its folder). tag_prefix: only
-- tags starting with it publish, the rest of the tag being the version (data-disk-v1.0.0 with prefix
-- "data-disk-"); '' takes every semver tag.
CREATE TABLE vcs_modules (
    namespace     text NOT NULL,
    name          text NOT NULL,
    system        text NOT NULL,
    connection_id uuid NOT NULL REFERENCES vcs_connections ON DELETE CASCADE,
    repo          text NOT NULL,
    dir           text NOT NULL DEFAULT '',
    synced_at     timestamptz,
    sync_error    text NOT NULL DEFAULT '',
    tag_prefix    text NOT NULL DEFAULT '',
    PRIMARY KEY (namespace, name, system)
);
CREATE INDEX vcs_modules_connection_idx ON vcs_modules (connection_id);

-- Audit log ------------------------------------------------------------------------------------

-- audit_log: append-only, each entry an HMAC (SECRET_KEY) over the previous hash and its content.
-- detail is json, not jsonb: its bytes are what the hash covers.
CREATE TABLE audit_log (
    id        bigint PRIMARY KEY,
    at        timestamptz NOT NULL,
    actor     text NOT NULL,
    action    text NOT NULL,
    detail    json NOT NULL,
    prev_hash bytea NOT NULL,
    hash      bytea NOT NULL
);
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END $$;
CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_immutable();

-- Views ----------------------------------------------------------------------------------------

-- The one definition of the computed columns lists and pages show; storage/queries select from these.

-- workspace_templates: the templates a workspace has or is getting: the blocking run's version while there is one
-- (drift checks and pull request plans run the current one anyway), else its current one. A run that ends without
-- its apply leaves the current one. Writes create versions (storage/configversions.go).
CREATE VIEW workspace_templates AS
SELECT w.id AS workspace_id, m.name, m.template_id, m.module_source, m.module_ref, m.inputs, m.follows, m.importing
FROM workspaces w JOIN configuration_modules m ON m.cv_id = COALESCE(
    (SELECT r.cv_id FROM runs r WHERE r.workspace_id = w.id AND run_blocking(r.status, r.action, r.vcs_pull_request)), w.current_cv_id);

-- workspace_list: a workspace with its project, creator, VCS link (vcs_* empty if none), number of
-- added templates, and drifted: the latest assessment found drift and nothing was applied since.
CREATE VIEW workspace_list AS
SELECT w.id, w.name, w.project_id, p.name AS project_name, w.created_by, COALESCE(u.email, '') AS created_by_email,
    w.auto_apply, w.drift_detection, w.global_remote_state, w.execution_mode, w.created_at,
    vcs.connection_id AS vcs_connection_id, COALESCE(vcs.repo, '') AS vcs_repo, COALESCE(vcs.branch, '') AS vcs_branch,
    COALESCE(vcs.trigger, false) AS vcs_trigger, COALESCE(vcs.pull_requests, false) AS vcs_pull_requests,
    COALESCE(vcs.tags_regex, '') AS vcs_tags_regex, COALESCE(vcs.tag_baseline, '') AS vcs_tag_baseline,
    (SELECT count(*) FROM workspace_templates t WHERE t.workspace_id = w.id)::bigint AS templates,
    COALESCE((SELECT a.status = 'drifted' AND NOT EXISTS (SELECT 1 FROM runs x WHERE x.workspace_id = w.id
            AND x.status = 'applied' AND x.updated_at > a.updated_at) FROM runs a
        WHERE a.workspace_id = w.id AND a.action = 'assess' AND a.status IN ('no_changes', 'drifted')
        ORDER BY a.created_at DESC LIMIT 1), false)::boolean AS drifted
FROM workspaces w
JOIN projects p ON p.id = w.project_id
LEFT JOIN users u ON u.id = w.created_by
LEFT JOIN workspace_vcs vcs ON vcs.workspace_id = w.id;

-- run_list: a run with the emails of who queued and who confirmed it ('' for none).
CREATE VIEW run_list AS
SELECT r.id, r.workspace_id, r.action, r.status, r.replace, r.plan, r.outputs, r.runner, r.cancel_requested,
    COALESCE(r.vcs_commit, '') AS vcs_commit, COALESCE(r.vcs_pull_request, 0)::integer AS vcs_pull_request,
    COALESCE(cu.email, '') AS created_by_email, COALESCE(fu.email, '') AS confirmed_by_email, r.created_at, r.updated_at,
    r.log_removed_at, r.log_bytes, COALESCE(r.confirm_reason, '') AS confirm_reason
FROM runs r
LEFT JOIN users cu ON cu.id = r.created_by
LEFT JOIN users fu ON fu.id = r.confirmed_by;

-- project_list: a project with whether it is the default, and its workspace and grant counts.
CREATE VIEW project_list AS
SELECT p.id, p.name, p.description, (p.id = o.default_project_id)::boolean AS is_default,
    (SELECT count(*) FROM workspaces w WHERE w.project_id = p.id)::bigint AS workspaces,
    (SELECT count(*) FROM project_access a WHERE a.project_id = p.id)::bigint AS grants, p.created_at, p.runner_pool
FROM projects p, organization o;

-- variable_set_list: a variable set with the names of the workspaces and projects it's attached to.
CREATE VIEW variable_set_list AS
SELECT vs.id, vs.name, vs.description, vs.global, vs.created_at,
    ARRAY(SELECT w.name FROM variable_set_workspaces a JOIN workspaces w ON w.id = a.workspace_id
        WHERE a.set_id = vs.id ORDER BY w.name)::text[] AS workspaces,
    ARRAY(SELECT p.name FROM variable_set_projects a JOIN projects p ON p.id = a.project_id
        WHERE a.set_id = vs.id ORDER BY p.name)::text[] AS projects,
    vs.priority
FROM variable_sets vs;

-- vcs_connection_list: a connection with its selected workspaces and projects (names) and what uses it.
CREATE VIEW vcs_connection_list AS
SELECT c.id, c.name, c.provider, c.base_url, c.token, c.workspace_scope, c.created_at,
    ARRAY(SELECT w.name FROM vcs_connection_workspaces a JOIN workspaces w ON w.id = a.workspace_id
        WHERE a.connection_id = c.id ORDER BY w.name)::text[] AS workspaces,
    ARRAY(SELECT p.name FROM vcs_connection_projects a JOIN projects p ON p.id = a.project_id
        WHERE a.connection_id = c.id ORDER BY p.name)::text[] AS projects,
    (SELECT count(*) FROM workspace_vcs w WHERE w.connection_id = c.id)::bigint AS linked_workspaces,
    (SELECT count(*) FROM vcs_modules m WHERE m.connection_id = c.id)::bigint AS linked_modules,
    EXISTS (SELECT 1 FROM template_catalog t WHERE t.connection_id = c.id)::boolean AS catalog
FROM vcs_connections c;

-- rollout_list: a rollout with its project's name and its creator's email ('' for none).
CREATE VIEW rollout_list AS
SELECT o.id, o.template_id, o.module_source, o.module_ref, o.project_id, COALESCE(p.name, '') AS project_name,
    o.batch_size, o.status, o.message, o.created_by, COALESCE(u.email, '') AS created_by_email, o.created_at, o.resumed_at
FROM template_rollouts o
LEFT JOIN projects p ON p.id = o.project_id
LEFT JOIN users u ON u.id = o.created_by;
