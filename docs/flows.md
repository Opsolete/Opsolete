# Flow inventory (1.0, Phase 8 step 1)

Every user flow of Opsolete, the tests that cover it, and the gaps. Not published (outside `docs/content`).

**Verdicts.** `full`: the flow end to end through the real HTTP stack (`Server.Handler()`, real Postgres; the
runner's calls made by the test against the real runner API), or through the real runner (`runner.Execute`
against `fakeServer`). For background work with no HTTP surface (scheduler, notifier, retention), `full` means
the real component against real Postgres. `partial`: pieces tested at app/storage/package level, or steps or
failure paths missing (named). `none`: no test.

**Cross-cutting fact.** No Go test runs the real runner against the real server: HTTP tests play the runner
(`claim`/`ack`/`finish` by hand), runner tests use `runner/helpers_test.go:fakeServer`. Only the dev tasks
`just seed` (real server + real runner + real Terraform, not run in CI) and the NixOS VM test
(`nix/test.nix`, boots both, runs no job) come close; `just smoke` only creates and lists a workspace.

Test paths are relative to `internal/`.

---

## A. Platform, login and access

**A1. First start + first admin login** — `full` (login), `partial` (startup)
- Actor: operator deploying; first admin (in `ADMIN_EMAILS`/`ADMIN_GROUPS`).
- Pre: Postgres, `SECRET_KEY` ≥ 32, `RUNNER_TOKEN` ≥ 32 and `STATE_STORE` (with deployments), OIDC or `AUTH_DEV_USER`.
- Steps: start `opsolete` (migrations, signing key, state store probe, catalog load, `CheckTemplates`) → open UI → redirected to `/auth/login` → IdP → `/auth/callback` → session cookie.
- Outcome: user row keyed by issuer+sub, groups recorded, role admin/user re-derived every request; sign-in audited.
- Failures: bad config (startup error); forged state/nonce, `email_verified:false`, email of another subject → 4xx; open redirect blocked; `preferred_username` never matches `ADMIN_EMAILS`.
- Coverage: `auth/auth_test.go:TestOIDCLogin`, `TestOIDCLoginRejects`, `TestSessionCookie`, `TestLocalPath`; `storage/storage_test.go:TestUpsertUserSubject`; `http/server_test.go:TestLoginRequired`; `config/config_test.go:TestLoad`; `http/server_test.go:TestFeatureSwitches` (CheckTemplates). Missing: `serve()` startup checks (RUNNER_TOKEN length, STATE_STORE probe, `AUTH_DEV_USER` warning) untested.

**A2. Logout** — `partial`
- Actor: any user. Steps: `POST /auth/logout`. Outcome: Opsolete session ends (IdP session stays).
- Coverage: `http/access_test.go:TestAccessMatrix/POST_/auth/logout` (303 only). Missing: cookie cleared / session no longer valid.

**A3. Login required, CSRF, CSP, security headers** — `full`
- Anonymous GET → 302 to login with `next`; anonymous POST → 401; cross-site POST → 403; strict CSP on every page.
- Coverage: `http/server_test.go:TestLoginRequired`, `TestCrossSitePostRejected`; `http/static_test.go:TestCSP`, `TestStatic`; `http/render_test.go:TestPageModels` (no inline handlers); `http/access_test.go:TestAccessMatrix` (every route × anonymous).

**A4. Team access by IdP groups (workspace grants)** — `full`
- Actor: admin grants; group members use. Pre: users logged in once (groups recorded).
- Steps: admin `POST /workspaces/{name}/access` group+level → members see (read) or operate; revoke = empty level.
- Outcome: no access = 404; read = view runs/logs/outputs, no settings/actions (403); operate = runs, settings, variables. Grant form offers groups seen at login.
- Failures: non-admin grants → 403; bad level → 422.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/groups_see_the_workspace_once_granted…`; `http/access_test.go:TestAccessMatrix` (reader/operator columns on every route); `app/app_test.go:TestWorkspaceAccess`; `storage/jobs_test.go:TestAccess`.

**A5. Feature switches (registry-only, deployments off, registry off)** — `full`
- `DEPLOYMENTS_ENABLED=false`: `/` → `/registry`, workspace/runner/cloud routes 404. `REGISTRY_ENABLED=false`: registry routes 404, registry templates refused (save and startup).
- Coverage: `http/server_test.go:TestFeatureSwitches`; `http/vcs_test.go:TestVCSWithoutRegistry`; `config/config_test.go:TestLoad`.

---

## B. Workspaces

**B1. Create a workspace from a template (deploy form)** — `full`
- Actor: admin, or user with operate on the chosen project (default project included). Pre: template in catalog.
- Steps: `/` → `/templates/{id}` → fill form (+ name, project) → `POST /templates/{id}`.
- Outcome: workspace in the project, template added as `module.<key>_<hex>`, first plan queued, redirect to the run.
- Failures: invalid name / reserved `new` / taken name / invalid input → 422 re-render with field errors; project not creatable → 422 on `ops-project`.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/create_workspace_from_a_template`; `http/forms_test.go:TestFormsRerender` (`POST /templates/{id}`); `http/templates_test.go:TestInvalidSubmitRerendersForm`; `http/access_test.go:TestAccessMatrix/POST_/templates/{id}`; `http/workspacetemplates_test.go:TestSensitiveInputs` (sealed inputs at create).

**B2. Create a blank workspace** — `full`
- Steps: `/workspaces/new` → `POST /workspaces/new` (name, project). Outcome: no run; "New run" hidden; a run is 409 (nothing to plan).
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/a_blank_workspace_has_no_template…`; `app/app_test.go:TestCreateEmptyWorkspaceAtomic`; `app/runs_test.go:TestStartRun` (`ErrNoConfig`).

**B3. Rename a workspace** — `full`
- Actor: operate. Steps: Settings › General `POST /workspaces/{name}/settings`. Outcome: new URL; runs, grants, variables kept; old name free.
- Failures: reader 403; invalid/taken name 422.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/rename_keeps…`; `storage/jobs_test.go:TestAccess` (taken name).

**B4. Delete a workspace** — `full`
- Actor: operate. Steps: Settings › Destruction and deletion `POST /workspaces/{name}/delete`.
- Outcome: runs, variables, grants, state files gone; 404 after.
- Failures: state manages resources → 409 unless `force=1`; active run → 409; reader 403, stranger 404.
- Coverage: `http/workspacetemplates_test.go:TestDeleteWorkspace`; `app/app_test.go:TestDeleteWorkspaceRemovesBlobs`, `TestWorkspaceAccess`; `http/access_test.go:TestAccessMatrix/POST_/workspaces/{name}/delete`.

**B5. Workspace settings pages (General, Health, Remote state)** — `full`
- Each page saves only its own fields; settings need operate (readers see a summary on the Overview).
- Coverage: `http/forms_test.go:TestSettingsPagesSaveOwnFields`, `TestFormsRerender`; `http/runs_test.go:TestWorkspaceFlow/settings_pages_and_workspace_tabs_render`; `http/access_test.go:TestAccessMatrix` (`GET /workspaces/{name}/settings*`: reader 403).

**B6. Lock a workspace from the UI** — `full`
- Actor: operate. Steps: Settings › Locking: lock (reason ≤ 300) / unlock (holder) / force unlock (other operator).
- Outcome: runs stay queued while locked, drift checks still run; unlock lets the queued run start.
- Failures: reason too long 422; locking a locked workspace 409; unlock by non-holder does nothing.
- Coverage: `http/settings_test.go:TestWorkspaceLock` (claims checked at store level); `http/forms_test.go:TestFormsRerender` (`/lock`).

**B7. Execution mode (remote ↔ local)** — `partial`
- Local: runs refused (409), CLI may lock/write state; remote: CLI lock/state upload refused.
- Coverage: `app/runs_test.go:TestStartRun` (local → `ErrLocalExecution`); `http/cloud_test.go:TestCloudLoginAndState` (mode switched via `store.SetExecutionMode`, then CLI refused). Missing: switching through `POST /workspaces/{name}/settings` and the UI's 409 for a run on a local workspace.

**B8. Workspace pages (overview, runs, states, variables, outputs, resources, templates)** — `full`
- Coverage: `http/runs_test.go:TestWorkspaceFlow/workspace_page_shows_resources,_outputs_and_templates`, `…/settings_pages_and_workspace_tabs_render`; `http/workspacetemplates_test.go:TestGroupResources`; `http/render_test.go:TestPageModels`.

---

## C. Runs

**C1. Plan → confirm → apply → outputs** — `full`
- Actor: operator; runner. Pre: workspace with configuration.
- Steps: `POST /workspaces/{name}/runs action=apply` (or create/configure) → runner claims plan, acks, locks state, uploads plan + lock file, finishes with summary/cost → `needs_confirmation` → `POST …/runs/{id}/confirm` → runner claims apply (same state password), downloads artifacts, writes state under lock, finishes with outputs + resources.
- Outcome: `applied`; state version indexed; sensitive outputs `(sensitive)`; artifacts and intermediate versions deleted; credential dead after the run.
- Failures: stranger confirm → 404; second run while one waits → 409; no claim while waiting; state access while waiting → 403; Terraform refuses a stale plan at apply.
- Coverage: `http/runs_test.go:TestWorkspaceFlow` (steps "plan job carries…" through "a finished apply indexes its state…", "plan page shows the changes and cost"); `runner/runner_test.go:TestPlanThenApply` (real Terraform: plan, apply, stale plan refused); `storage/jobs_test.go:TestRunLifecycle`; `app/runs_test.go:TestRunEndDeletesArtifacts`.

**C2. Auto-apply** — `partial`
- Workspace setting; an apply plan with changes goes straight to `apply_queued`. Destroys, refresh-only and rollout plans still ask.
- Coverage: `storage/jobs_test.go:TestAutoApplyAndDiscard` (store only); `http/runs_test.go:TestWorkspaceFlow/a_destroy_plan_asks_even_with_auto-apply…` (only the destroy exception). Missing: an apply auto-applied through HTTP; refresh-only exception.

**C3. Discard** — `full`
- `POST …/runs/{id}/discard` → `discarded`, plan artifacts deleted, workspace free.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_destroy_plan_asks_even_with_auto-apply;_discarding_deletes_it`; `app/runs_test.go:TestRunEndDeletesArtifacts`; `storage/jobs_test.go:TestAutoApplyAndDiscard`.

**C4. Destroy** — `full`
- Settings › Destruction: `action=destroy` → plan -destroy → always asks → confirm → apply → workspace shows `destroyed`, no resources.
- Failures: stranger 404; reader 403.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_confirmed_destroy_empties_the_state`; `runner/runner_test.go:TestPlanThenApply` (destroy phase); `http/variables_test.go:TestTeamsVariablesRenameCancel` (reader 403).

**C5. Refresh-only run** — `none`
- `action=refresh` → `plan -refresh-only` → always asks (even with auto-apply) → apply updates state only.
- Coverage: only refusals: `http/runs_test.go:TestWorkspaceFlow/replace_sends_only_addresses…` (refresh + replace → 422). The runner's `-refresh-only` path is exercised only by drift checks (`runner/runner_test.go:TestPlanThenApply`, assess).

**C6. Replace resources** — `full`
- Tick resources → `action=apply replace=<addr>` → job carries `Replace` → plan shows replace.
- Failures: unknown address 422; replace with non-apply action 422.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/replace_sends_only_addresses…`; `runner/runner_test.go:TestPlanThenApply` (replace); `app/runs_test.go:TestStartRun`.

**C7. Plan only (speculative, by hand)** — `full`
- `action=plan` → plan → ends `planned`, never applied.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_plan-only_run_with_changes_ends_as_planned`.

**C8. One active run per workspace** — `full`
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_plan_waiting_for_confirmation_holds_the_workspace…`; `storage/jobs_test.go:TestRunLifecycle`, `TestConcurrentTriggers`; `app/runs_test.go:TestStartRun`.

**C9. Cancel a queued run** — `partial`
- `POST …/cancel` on a queued run → `canceled` at once, artifacts deleted, configuration change undone.
- Coverage: `app/runs_test.go:TestRunEndDeletesArtifacts` (cancel), `storage/jobs_test.go:TestCancel`, `storage/runconfig_test.go:TestRunConfigRestore`; HTTP only as a 303 in `http/access_test.go:TestAccessMatrix/POST_/workspaces/{name}/runs/{run}/cancel`.

**C10. Cancel a running run (graceful)** — `partial`
- Operator cancels → next heartbeat/log post answers `StatusCancel` (lease kept) → runner SIGINTs Terraform, which saves state → runner finishes `canceled` → run `canceled`, lock released.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/cancel_reaches_the_runner_on_its_heartbeat…` (heartbeat answer only); `storage/jobs_test.go:TestCancel` (finish canceled); `runner/terraform_test.go:TestCancelSavesState`, `TestInterrupt/cancel_*`. Missing: the server half end to end (runner finish `canceled` through HTTP, state saved during cancel, run page shows it).

**C11. Force cancel** — `full`
- After a pending cancel, `force=1` → run `canceled`, runner fenced off (409), state lock and password dead.
- Failures: cancel of a finished run → 409.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/cancel_reaches…,_force_cancel_cuts_it_off`; `storage/jobs_test.go:TestCancel` (lock released), `TestStateWriteFenced`; `app/state_test.go:TestStateRunCredential` (credential dead).

**C12. Follow a run's log (live SSE, raw log, reconnect, cap)** — `partial`
- Run page streams `…/logs/stream` (SSE, `Last-Event-ID` resume, `done` event); raw `…/logs`; max 8 streams per user per replica (429).
- Coverage: `http/runs_test.go:TestWorkspaceFlow/raw_log_and_SSE_stream_replay_the_run's_log` (ended run only); `http/logs_test.go:TestLogStreamCap`; `app/notify_test.go:TestNotifyDelivery/a_run's_log_and_status_wake_its_own_subscribers_only`. Missing: an open stream woken by a new chunk while the run is active, through HTTP.

**C13. Cost estimation** — `partial`
- Runner with `OPS_INFRACOST_API_KEY` prices plans; run page shows cost; currency from Settings › Organization.
- Coverage: `runner/infracost_test.go:TestEstimate`; `http/runs_test.go:TestWorkspaceFlow/plan_page_shows_the_changes_and_cost` (cost in finish → page); `http/organization_test.go:TestOrganizationCurrency`. Missing: the currency reaching the job (unverified how).

---

## D. Templates on a workspace

**D1. Add a template to a workspace** — `full`
- Actor: operate. Steps: Deploy… → `/workspaces/{name}/templates/new?template=id` → form → plan queued.
- Outcome: `module.<key>_<hex>` (key input, else template name), several allowed, same key twice → distinct names.
- Failures: unknown template 404; invalid input 422 (add form re-rendered); active run 409.
- Coverage: `http/workspacetemplates_test.go:TestWorkspaceTemplates`; `http/forms_test.go:TestFormsRerender`; `app/templates_test.go:TestModuleName`, `TestDefaultModule`; `runner/assemble_test.go:TestAddedTemplates`, `TestEscapeValue`.

**D2. Configure a template / update it to a new module version** — `full`
- Templates tab shows `update: <ver>`; Configure… shows inputs + new ones; save moves to the template's version and queues a plan.
- Failures: configure during an active run → 409, inputs unchanged.
- Coverage: `http/vcs_test.go:TestVCSPublishAndUpdate/the_workspace's_template_updates_to_the_new_version…`; `http/workspacetemplates_test.go:TestWorkspaceTemplates` (configure); `http/workspacetemplates_test.go:TestSensitiveInputs` (empty sensitive kept).

**D3. Remove a template** — `partial`
- Resources deployed → destroy run targeted at `module.<name>`; on apply the template goes and an outputs-only cleanup plan (`-refresh=false`) drops its outputs. Last one → plain destroy. Nothing deployed → removed at once, no run.
- Failures: removing twice → 404; active run → 409.
- Coverage: `http/workspacetemplates_test.go:TestWorkspaceTemplates` (destroy job + target via HTTP, but finished through `store.FinishJob`; immediate removal); `runner/assemble_test.go:TestAddedTemplates` (targeted destroy, outputs-only apply). Missing: the destroy confirmed and applied through the runner API with the template gone afterwards.

**D4. Configuration restored when a run ends before its apply** — `partial`
- Adding/configuring/rollout run discarded, canceled or errored while planning → previous version and inputs back (added template removed); once apply started the new one stays; a later change isn't overwritten.
- Coverage: `storage/runconfig_test.go:TestRunConfigRestore`; `app/rollouts_test.go:TestTickRollout` (discard restores); `storage/rollout_test.go:TestUpgradeTemplates`. Missing: through HTTP (discard / cancel of a configure run, Templates tab shows the old version).

**D5. Sensitive template inputs** — `full`
- Password field, sealed bound to workspace+module, `(sensitive)` on pages, `(redacted)` in audit, kept when left empty, clear in the job; a moved sealed value fails the job.
- Coverage: `http/workspacetemplates_test.go:TestSensitiveInputs`; `app/sealing_test.go:TestVariableAD`; `app/inputs_test.go:TestSealInputsKeepsRejectedError`; `registry/registry_test.go:TestResolveSensitive`.

**D6. Form rules (validation, help, show_if, defaults)** — `full`
- Coverage: `http/templates_test.go:TestInvalidSubmitRerendersForm`; `http/render_test.go:TestRenderShowIf`; `templates/templates_test.go:TestShowIf`, `TestShowIfBool`, `TestValidateEdgeCases`, `TestLoadCatalogAndValidate`; `templates/json_test.go:TestSchema`, `TestJSONRoundTrip`.

---

## E. State

**E1. State backend for runs** — `full`
- Runner uses `http` backend with HMAC(workspace, run) credential, valid only while planning/applying and acknowledged. Plans lock but never write; applies write under their lock, newer serial of one lineage only; unchanged writes no-op; 423 with holder info on conflict; state and plans sealed in `STATE_STORE`.
- Coverage: `http/runs_test.go:TestWorkspaceFlow` ("a plan locks the state but never writes it", "apply writes state under its lock…", "a finished apply indexes its state…"); `app/state_test.go:TestStateRunCredential`; `storage/jobs_test.go:TestStateWriteFenced`, `TestFinishKeepsForeignStateLock`, `TestExpiredLeaseIsErrored`; `app/sealing_test.go:TestBlobEncryption`; `http/access_test.go:TestAccessMatrix` (state rows).

**E2. State versions and diffs** — `full`
- States tab lists versions (read); a version page shows the diff to the previous one and what a rollback changes (operate only: values).
- Coverage: `http/runs_test.go:TestWorkspaceFlow/rollback_copies_v2…` (page, reader 403, stranger 404); `app/state_test.go:TestStateDiffAccess`; `app/statediff_test.go:TestDiffStates`.

**E3. Rollback** — `full`
- Operate; type "rollback"; copies an old version as current (serial+1, `rollback to vN`) with outputs and resource index; takes and releases the state lock.
- Failures: current version 409; unknown 404; stranger 404; active run / locked → 409; lock released under it → `ErrLockLost`, nothing recorded.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/rollback_copies_v2,_its_outputs_and_resource_index,_as_v4`; `app/state_test.go:TestRollback`; `storage/jobs_test.go:TestRollbackWriteFenced`.

**E4. Remote state sharing** — `full`
- Settings › General: share with all or specific workspaces; consumer runs read (never lock/write); PR speculative plans never read shared state.
- Coverage: `http/state_test.go:TestRemoteStateSharing`; `app/state_test.go:TestStateRunCredential`.

**E5. Orphaned locks released** — `partial`
- Leader job every 15 s: a run's lock without a lease, a rollback's lock older than `STALE_LOCK_TIMEOUT`; CLI/UI locks stay.
- Coverage: `storage/jobs_test.go:TestReleaseOrphanedLocks`. Missing: `app.ReleaseOrphanedLocks` (logging, notify) and the effect seen through the API.

---

## F. Terraform CLI (`cloud` block)

**F1. `terraform login`** — `full`
- `/.well-known/terraform.json` → `GET/POST /oauth/authorization` (consent, PKCE S256, localhost redirect) → `POST /oauth/token` → 30-day user token.
- Failures: foreign redirect_uri 400; wrong verifier 400.
- Coverage: `http/cloud_test.go:TestCloudLoginAndState`; `http/access_test.go:TestAccessMatrix` (oauth rows); `http/static_test.go:TestCSP` (authorize page form-action).

**F2. `terraform init` with `cloud {}`: find or create the workspace (and project)** — `full`
- Entitlements; read workspace by name; create a missing one (local execution, owned by the user) in `project` or the default project; projects list by name; admins create missing projects.
- Failures: other org 404; no operate on the project (default included) → 422 "operate"; non-admin project create → 403; no token 401.
- Coverage: `http/cloud_gotfe_test.go:TestCloudGoTFE` (go-tfe as Terraform 1.14 ships it); `http/cloud_test.go:TestCloudLoginAndState`; `http/access_test.go:TestAccessMatrix` (cloud rows).

**F3. Local plan/apply with remote state: lock, upload, download, outputs, unlock** — `full`
- Failures: save without lock 409; second lock 409 (`ErrWorkspaceLocked`); unlock of another user's lock → `ErrWorkspaceLockedByUser`; of a run's lock → `ErrWorkspaceLockedByRun`; upload-URL flow → 422 fallback to inline; outputs list names only; reader → 403; stranger → 404.
- Coverage: `http/cloud_gotfe_test.go:TestCloudGoTFE`; `http/cloud_test.go:TestCloudLoginAndState`; `storage/jobs_test.go:TestCloudStateWriteFenced`.

**F4. `terraform force-unlock`** — `partial`
- Releases a CLI lock (any operator); never a run's or rollback's.
- Coverage: `storage/jobs_test.go:TestCloudStateWriteFenced` (a force-unlock racing an upload). Missing: `actions/force-unlock` through the API (go-tfe `ForceUnlock`), and its refusal on a run lock.

**F5. Remote-execution workspace refuses the CLI** — `full`
- Coverage: `http/cloud_test.go:TestCloudLoginAndState` (lock and state upload → 409 "remote execution"; run on a local workspace → `ErrLocalExecution`).

---

## G. Variables and variable sets

**G1. Workspace variables** — `full`
- Operate: add env/terraform, sensitive write-only; reserved env names refused (`PATH`, `TF_*`, `GIT_*`, `OPS_*` but `OPS_DEBUG`); delete. Values reach the claimed job.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/operators_set_workspace_variables…`, `…/the_runner's_job_carries…`; `app/variables_test.go:TestVariableAccess`; `http/audit_test.go:TestAuditLogRecordsChanges` (value redacted); `runner/runner_test.go:TestJobEnv`, `TestCompactWarnings` (`OPS_DEBUG`).

**G2. Variable sets (admin)** — `partial`
- Create (optionally for a project or workspace), add/delete variables, attach to workspaces and projects, global flag, delete set.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel` (create, vars, attach ws, non-admin 403, unknown ws 422); `http/projects_test.go:TestProjectPages` (attach/detach projects, create for project/workspace); `http/projects_test.go:TestAttachProjectsParsesForm`; `storage/jobs_test.go:TestRunVariables`. Missing through HTTP: making a set global (`POST /variable-sets/{set}`), deleting a set (only matrix 303), deleting a set variable.

**G3. Variable precedence** — `partial`
- template inputs > workspace vars > sets on the workspace > sets on its project > global sets; same kind: first by name.
- Coverage: `storage/jobs_test.go:TestRunVariables` (all set tiers); `app/variables_test.go:TestWithWorkspaceVars` (template inputs win; registry modules only); `http/variables_test.go:TestTeamsVariablesRenameCancel` (workspace var beats an attached set, in the job). Missing: project and global tiers in a claimed job through HTTP.

**G4. VCS-linked workspaces and sets (security rule)** — `partial`
- Linked workspaces get no global or project sets; changing repo/branch, switching to/changing tag triggers, or turning on PR plans detaches the workspace's own sets.
- Coverage: `storage/jobs_test.go:TestVCSWorkspaceVariableSets`, `TestRunVariables/a_VCS-linked_workspace_gets_only_the_sets_attached_to_it`. Missing: through HTTP (`POST /workspaces/{name}/vcs` then the claimed job's `Env`).

---

## H. Projects

**H1. Create, rename, delete a project** — `full`
- Admin; name rule (`new` reserved); only empty, non-default projects deleted (grants and set attachments go with it).
- Coverage: `http/projects_test.go:TestProjectPages`; `app/projects_test.go:TestProjects`; `storage/projects_test.go:TestProjects`.

**H2. Project grants** — `full`
- Grant reaches every workspace; highest of creator / workspace / project level; project visible with a grant or a visible workspace; default project visible to all.
- Coverage: `http/projects_test.go:TestProjectPages`; `app/projects_test.go:TestProjects`; `storage/jobs_test.go:TestAccess`.

**H3. Move a workspace to another project** — `full`
- Admin only, allowed during a run; the workspace loses project-scoped VCS connections.
- Coverage: `http/projects_test.go:TestProjectPages`; `http/workspaceconfig_test.go:TestVCSProjectScope`; `app/projects_test.go:TestProjects`. (Audit entry only via the generic request audit.)

**H4. Who may create workspaces** — `full`
- Admins anywhere; others only where a group has operate, the default project included; same rule for UI and cloud API; refusals don't tell whether the project exists.
- Coverage: `app/projects_test.go:TestNewWorkspaceProject`; `http/projects_test.go:TestProjectPages`; `http/cloud_gotfe_test.go:TestCloudGoTFE`; `http/access_test.go:TestAccessMatrix` (`POST /workspaces/new`, `POST /templates/{id}`).

**H5. Make another project the default** — `none`
- `POST /projects/{project}/settings` with default → `organization.default_project_id`; new workspaces and the cloud API without a project land there.
- Coverage: none (`app.UpdateProject` Default path and `Store.SetDefaultProject` untested).

---

## I. VCS

**I1. Connect a provider** — `full` (GitHub), `partial` (Azure DevOps)
- Admin; wizard (also from new-workspace, publish, catalog); token checked by listing repos, sealed, never shown; scope off by default.
- Failures: bad token 422 "connection check failed"; taken name 422; non-admin 403.
- Coverage: `http/vcs_test.go:TestVCSPublishAndUpdate/connecting_checks_the_token…`, `TestVCSAdminOnly`; `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/the_add-VCS_wizard…`; `app/vcs_test.go:TestVCSConnectionsAdminOnly`. Azure DevOps: `vcs/azuredevops_test.go:TestAzureDevOpsRequests`, `vcs/vcs_test.go:TestProviders`, `vcs/writes_test.go:TestAzureDevOpsWrites` (package level only).

**I2. Connection scope (off / all / selected workspaces and projects)** — `full`
- Checked at link time and at each run start; deselected workspace's run fails ("may not link…"), page warns.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces` ("a new connection links workspaces only once…", "an admin narrows…", "a deselected workspace's run fails…"), `TestVCSProjectScope`; `app/vcs_test.go:TestLinkVCSScope`; `storage/projects_test.go:TestVCSConnectionAllows`.

**I3. Replace a connection's token** — `full`
- Coverage: `http/catalog_test.go:TestTemplateCatalogInGit/the_connection's_token_is_replaced…`; `app/catalog_test.go:TestReplaceVCSTokenRace`.

**I4. Delete a connection** — `partial`
- Unlinks its workspaces; refused while a linked workspace has an active run or the catalog uses it.
- Coverage: `app/vcs_test.go:TestVCSConnectionsAdminOnly` (unlinks), `app/catalog_test.go:TestDeleteVCSConnectionDuringRun`; `http/catalog_test.go:TestTemplateCatalogInGit/the_connection_can't_be_deleted_under_the_catalog…`. Missing: a successful delete through HTTP and its effect on linked workspaces.

**I5. Link a workspace to a branch; change source; unlink** — `full`
- Operate; at create (`/workspaces/new?type=vcs`) or Version control › Change source; branch checked; runs plan the pinned commit via a signed archive; relink during a run 409; unlink (`type=cli`).
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces` ("a linked workspace plans its branch head…", "a linked workspace shows its source…", unlink); `app/vcs_test.go:TestLinkVCSDuringRun`, `TestVCSArchiveSignature`; `app/app_test.go:TestCreateEmptyWorkspaceAtomic`.

**I6. Push → run** — `partial`
- Leader polls each branch once a minute; each new commit without a run queues an apply run of it; only the newest if several landed.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/commits_queue_apply_runs…` (calls `App.TriggerVCSRuns` directly); `storage/jobs_test.go:TestQueueVCSRun`, `TestConcurrentTriggers`. Missing: `App.PollVCSRuns` (grouping by branch, errors, pauses) untested.

**I7. Pull request → speculative plan** — `partial`
- Opt-in; each new head commit of an open PR into the branch gets a plan-only run; forks skipped; never applied; no shared state.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/commits_queue_apply_runs,_pull_requests_speculative_plans`; `storage/jobs_test.go:TestQueueVCSRun` (ends `planned`); `vcs/vcs_test.go:TestProviders` (forks left out); `http/state_test.go:TestRemoteStateSharing`. Missing: poll loop (as I6).

**I8. Tag-based triggers** — `partial`
- Format (semantic, v-prefix, suffix, custom regex); newest matching tag at switch time is the baseline and never runs; later tags queue apply runs; manual runs plan the newest tag; PR plans off.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/tag_triggers_run_only_tags_pushed_after…`; `app/vcsruns_test.go:TestLatestTag`. Missing: manual run of a tag-based workspace planning the newest tag; poll loop.

**I9. Polling cost control (ETags, pauses, rate limits)** — `partial`
- Coverage: `vcs/vcs_test.go:TestConditionalRequestsAndPause`, `TestRateLimitedResponsePauses` (package level).

---

## J. Module registry

**J1. Upload a module** — `full`
- Admin, with Settings › Organization › Module uploads on (or `TEMPLATE_EDITOR`); `.tar.gz`, semver; immutable; docs generated.
- Failures: uploads off / non-admin 403; re-upload 422 "immutable"; bad name 422.
- Coverage: `http/registry_test.go:TestRegistry`; `http/catalog_test.go:TestModuleUploadsSetting`; `http/builder_test.go:TestEditorIsAdminOnly`; `app/catalog_test.go:TestUploadModuleRefused`; `registry/registry_test.go:TestInspectAndInputs`, `TestInspectRejects`.

**J2. Publish from VCS tags; sync; unlink** — `full` (manual sync), `partial` (scheduled)
- Wizard (connection → repo → name/system from `terraform-<system>-<name>`, optional folder); semver tags become versions; folder missing in older tags skipped; Sync now; unlink keeps versions.
- Failures: folder escaping the repo 422; missing folder → sync error shown; unlink twice 404.
- Coverage: `http/vcs_test.go:TestVCSPublishAndUpdate`; `app/vcssync_test.go:TestNoFolderCache` (the leader job `PollVCSModules`, cache only).

**J3. Module docs pages** — `full`
- Coverage: `http/registry_test.go:TestRegistry` (HTML, README escaped, `docs.md`, unknown version 404).

**J4. Delete a module** — `full`
- Refused while a template uses it (409); removes every version; 404 twice.
- Coverage: `http/registry_test.go:TestRegistry`.

**J5. `terraform init` against the registry** — `full`
- Registry token (`opsolete token`) or `terraform login` token; versions → download (`X-Terraform-Get` signed 10-min URL) → archive; tampered signature 403.
- Coverage: `http/registry_test.go:TestRegistry`; `http/access_test.go:TestAccessMatrix` (registry rows, user token allowed); `registry/registry_test.go:TestAddressAndSigning`.

**J6. Registry module in a template (no-code)** — `full`
- Builder "Start from a registry module" prefills fields; module page shows *No-code ready* + Provision workspace; runner gets a signed source.
- Coverage: `http/registry_test.go:TestRegistry` (builder prefill, unknown version refused); `http/vcs_test.go:TestVCSPublishAndUpdate` (no-code ready, provision); `http/workspacetemplates_test.go:TestWorkspaceTemplates` (job source); `registry/registry_test.go:TestResolve`.

---

## K. Template catalog and builder

**K1. Builder in `TEMPLATES_DIR` editor mode** — `full`
- Admin, `TEMPLATE_EDITOR=1`; new (UUID id), edit, preview (saves nothing), delete; 1.0 format written; off → 404.
- Coverage: `http/builder_test.go:TestTemplateBuilder`, `TestBuilderRoundTrip`, `TestEditorIsAdminOnly`; `app/catalog_test.go:TestCatalogTemplatesAdminOnly`; `templates/templates_test.go:TestCatalogSave`.

**K2. Catalog in Git: set up, sync, invalid commit, multi-replica reload** — `full` (single process)
- Settings › Templates: connection/repo/branch/folder/mode; leader syncs every minute and on Sync now; invalid commit keeps the last good one and shows the error; snapshot in Postgres, every replica reloads within 10 s; older sync never overwrites a newer snapshot.
- Coverage: `http/catalog_test.go:TestTemplateCatalogInGit` (all of the above, two `Server`s on one DB); `storage/catalog_test.go:TestStaleCatalogSync`, `TestFailedCatalogSyncKeepsSyncedAt`; `app/catalogsync_test.go:TestCatalogFiles`; `templates/templates_test.go:TestParseLimits`.

**K3. Builder, commit mode** — `full`
- Save commits `<folder>/<id>.json`; stale file → refused, catalog reloads, "review and save again"; delete commits the removal.
- Coverage: `http/catalog_test.go:TestTemplateCatalogInGit` (steps "the builder commits…", "a save based on an outdated file…", "deleting in commit mode…"); `vcs/writes_test.go:TestGitHubWrites`, `TestAzureDevOpsWrites`.

**K4. Builder, pull request mode** — `partial`
- New template → branch `opsolete/template-<id>-…` + PR; saving again adds a commit to that PR; deleting always opens its own PR.
- Coverage: `http/catalog_test.go:TestTemplateCatalogInGit/pull_request_mode_opens_one_pull_request…`. Missing: delete in PR mode.

**K5. Disconnect the Git catalog** — `full`
- Coverage: `http/catalog_test.go:TestTemplateCatalogInGit/the_connection_can't_be_deleted…;_disconnecting_empties_it`.

---

## L. Template rollouts

**L1. Start a rollout** — `partial`
- Admin; Roll out on the template; scope (project or all), batch size (0 = all); must name the version the page showed.
- Failures: stale version 409; negative batch 422; non-admin 403; nothing to do → `done` at once.
- Coverage: `http/rollout_test.go:TestStartRollout`, `TestRenderRollout`; `http/forms_test.go:TestFormsRerender` (batch); `app/rollouts_test.go:TestRolloutsAdminOnly`. Missing: a rollout with workspaces to upgrade started through HTTP.

**L2. Rollout progress (batches, busy, needs input, local, done)** — `partial`
- Leader tick every 15 s; plans always ask even on auto-apply; busy retried; needs-input/local skipped.
- Coverage: `app/rollouts_test.go:TestRolloutPlan`, `TestTickRollout`; `storage/rollout_test.go:TestUpgradeTemplates`. Missing: through HTTP; `App.TickRollouts` (the job wrapper) untested.

**L3. Pause on failure, resume, stop** — `partial`
- Own run errored/discarded/canceled → paused; Resume retries; Stop ends it; template moved to another version → stopped.
- Coverage: `app/rollouts_test.go:TestTickRollout` (pause on discard, resume via store, stopped on version change); `storage/rollout_test.go:TestUpgradeTemplates`; `http/rollout_test.go:TestStartRollout` (pause of a done rollout → 409). Missing: `POST /templates/{id}/rollout/{pause|resume|stop}` on an open rollout.

**L4. Rollout audit entries** — `none`
- One `template rollout` entry per upgraded workspace, in the admin's name. No test reads it.

---

## M. Drift detection

**M1. Scheduled drift checks** — `partial`
- Settings › Health on → leader queues a refresh-only assess run once a day per workspace that manages resources; never applied; not in the runs list; banner + *drifted* tag; runs in a locked workspace.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/drift_check_is_scheduled,_never_applied…` (queued via `store.QueueAssessments`); `http/settings_test.go:TestWorkspaceLock`; `runner/runner_test.go:TestPlanThenApply` (assess plan). Missing: `App.QueueDriftChecks`; drift cleared by the next check or apply.

**M2. Check for drift now** — `none`
- `POST /workspaces/{name}/runs action=assess` (Overview, "Check for drift"). No test starts one.

---

## N. Audit log

**N1. View the audit log (changes, sign-ins and refusals)** — `full`
- Admin; every state-changing request, sign-ins, 401/403 incl. reads; principal, IP, form (secrets `(redacted)`, values capped); refusals on `?show=access`.
- Coverage: `http/audit_test.go:TestAuditLogRecordsChanges`, `TestAuditCapsForms`, `TestAuditRowSummary`; `auth/auth_test.go:TestOIDCLogin` (sign-in principal); `http/workspacetemplates_test.go:TestSensitiveInputs` (redaction).

**N2. Verify / tamper evidence** — `partial`
- Append-only table; HMAC chain under `SECRET_KEY`; page re-verifies and names the first broken entry; `SECRET_KEY_PREVIOUS` verifies pre-rotation entries.
- Coverage: `storage/audit_test.go:TestAuditLogAppendOnly`, `TestAuditLogKeyRotation`; `http/audit_test.go:TestAuditLogRecordsChanges` (page says "intact"). Missing: the page naming a broken entry.

---

## O. Account and organization

**O1. Profile** — `full` — `http/account_test.go:TestAccountTokens` (email, groups, account menu, unknown page 404).

**O2. Tokens: list, revoke one, revoke all** — `full`
- Only own tokens listed/revoked; revoke-all leaves other users'.
- Coverage: `http/account_test.go:TestAccountTokens`; `app/catalog_test.go:TestRevokeOwnTokensOnly`.

**O3. Organization settings: currency, module uploads** — `full`
- Coverage: `http/organization_test.go:TestOrganizationCurrency`, `TestSettingsNav`; `http/catalog_test.go:TestModuleUploadsSetting`; `app/catalog_test.go:TestUploadModuleRefused`.

---

## P. Operations

**P1. Retention** — `full`
- Hourly leader job: logs deleted `RUN_LOG_RETENTION_DAYS` after the run ended (page/raw/stream say so); assess and speculative plans deleted after 30 days except the latest per rules; never active runs; batches of 1000.
- Coverage: `app/retention_test.go:TestRetention`, `TestRetentionBatches`; `http/runs_test.go:TestRemovedLog`; `config/config_test.go:TestLoad`.

**P2. Key rotation (`opsolete rotate-key`)** — `partial`
- Restart with both keys → rotate (re-seal variables, VCS tokens, sensitive inputs, state files, plans; refuse with an active run; idempotent) → drop `SECRET_KEY_PREVIOUS`.
- Coverage: `app/rotate_test.go:TestRotateKey`; `secrets/secrets_test.go:TestRotation`; `storage/audit_test.go:TestAuditLogKeyRotation`. Missing: the CLI (`cmd/opsolete:rotateKey`: env checks, audit entry); "a value changed meanwhile" failure path.

**P3. Registry token from the CLI (`opsolete token <name>`)** — `none`
- Prints the token on stdout only, audits `create token`. `cmd/opsolete` has no tests.

**P4. Health, readiness, metrics** — `partial`
- `/healthz`, `/readyz` (503 without DB), `METRICS_ADDR` listener with request/run/DB metrics and `opsolete_scheduler_leader`.
- Coverage: `http/server_test.go:TestPublicPages`; `http/metrics_test.go:TestRequestMetricsUseRoutePattern`, `TestDatabaseMetrics`; `http/access_test.go:TestAccessMatrix` (healthz/readyz 200). Missing: readyz 503; the separate metrics listener.

**P5. Scheduler leader and failover (multi-replica)** — `full`
- One advisory-lock leader runs non-Local jobs; dead session → jobs cancelled, another replica leads within the retry; Local jobs everywhere; jobs never overlap, survive panics.
- Coverage: `app/scheduler_test.go:TestSchedulerLeader`, `TestRunJobs`. (The job list in `cmd/opsolete:jobs()` itself is untested.)

**P6. Notifications (LISTEN/NOTIFY)** — `full`
- Queueing wakes claims; log/status wake their run's streams; lock release wakes claims; rolled-back notifications not delivered; reconnect with fallback polling.
- Coverage: `app/notify_test.go:TestNotifyDelivery`, `TestNotifyReconnect`; `http/logs_test.go:TestClaimWakesOnQueue`; `storage/jobs_test.go:TestUnclaim` (unclaim wakes).

---

## Q. Runner lifecycle

**Q1. Runner start and configuration** — `partial`
- `opsolete runner` with `OPS_SERVER_URL`, `OPS_RUNNER_TOKEN` (scrubbed from env), timeouts; non-dumpable, child subreaper (refuses to start without).
- Coverage: `config/config_test.go:TestLoadRunner`; NixOS VM test `nix/test.nix` (outside `go test`). Missing: startup refusal when subreaper fails (unverified testable).

**Q2. Claim, acknowledge, heartbeat, log, artifacts, finish** — `full`
- Only the runner token claims (registry token / wrong token 401); long-poll ≤ 20 s; lease refused until acked; one job per phase; plan + lock file sealed between phases; invalid status 400; unknown id = lease lost (409).
- Coverage: `http/runs_test.go:TestWorkspaceFlow` (runner steps); `http/access_test.go:TestAccessMatrix` (runner rows); `app/runs_test.go:TestUnacknowledgedClaim`, `TestPutArtifactRace`; `storage/jobs_test.go:TestRunLifecycle`; `runner/runner_test.go:TestPlanThenApply`; `runner/client_test.go:TestClaimLargeJob`.

**Q3. Unacknowledged claim goes back to the queue** — `full`
- Not acked within 30 s → planning→plan_queued / applying→apply_queued, history names the runner, late ack fails; undecodable claim not acked by the runner.
- Coverage: `app/runs_test.go:TestUnacknowledgedClaim` (via `ReapLeases`); `storage/jobs_test.go:TestUnclaim`; `runner/runner_test.go:TestUnreadableClaim`.

**Q4. Lease loss (runner disappears; runner fenced off)** — `partial`
- Server: no heartbeat for the lease TTL → run errored ("runner stopped responding"), lock released, late writes 409, artifacts deleted. Runner: a 409 interrupts Terraform and reports nothing.
- Coverage: `storage/jobs_test.go:TestExpiredLeaseIsErrored`; `app/runs_test.go:TestRunEndDeletesArtifacts` (reap); `runner/terraform_test.go:TestLostLeaseStopsTerraform`. Missing: both halves together; the run page's message.

**Q5. Drain / scale down (first SIGTERM)** — `full`
- Stops claiming; a claim in flight completes, is acked and run; then the runner exits.
- Coverage: `runner/runner_test.go:TestDrainKeepsClaimedJob`. (The signal handler in `cmd/opsolete:runRunner` is untested.)

**Q6. Interrupts: timeout + grace, cancel, second SIGTERM** — `full`
- SIGINT, then kill after `OPS_JOB_GRACE`; messages name cause and outcome; no logs/heartbeats past the hard end.
- Coverage: `runner/terraform_test.go:TestInterrupt` (all causes × clean/killed, stall between commands); `runner/ship_test.go:TestShipperStopsAtHardEnd`; `config/config_test.go:TestLoadRunner`.

**Q7. Job isolation and cleanup** — `full` (Linux)
- Own temp dir and `HOME`; job env on top of runner env, `OPS_*` scrubbed; every leftover process killed after each job.
- Coverage: `runner/runner_test.go:TestJobHome`, `TestJobEnv`; `runner/isolate_linux_test.go:TestJobProcessesKilled` (Linux only; `just test-linux`).

**Q8. Log shipping, masking, caps** — `partial`
- ~1 s batches cut at line ends; sensitive values and signed URLs masked; 10 MB per run; 256 KB per post.
- Coverage: `runner/ship_test.go:TestShipperRedactsAndDrains` (signed URLs; `Job.Secrets` whole, per line, short ones kept); `runner/terraform_test.go:TestHeadBuffer`; `storage/jobs_test.go:TestRunLifecycle` (cap via `AppendLog` max); `runner/runner_test.go:TestPlanThenApply` (no leaks). Missing: the server filling `Job.Secrets` from sensitive variables and inputs (no test reads it); the 256 KB per-post limit (413).

---

## Counts

| Area | Flows | full | partial | none |
|---|---|---|---|---|
| A Platform, login, access | 5 | 4 | 1 | 0 |
| B Workspaces | 8 | 7 | 1 | 0 |
| C Runs | 13 | 7 | 5 | 1 |
| D Templates on a workspace | 6 | 4 | 2 | 0 |
| E State | 5 | 4 | 1 | 0 |
| F Terraform CLI | 5 | 4 | 1 | 0 |
| G Variables | 4 | 1 | 3 | 0 |
| H Projects | 5 | 4 | 0 | 1 |
| I VCS | 9 | 4 | 5 | 0 |
| J Registry | 6 | 6 | 0 | 0 |
| K Catalog and builder | 5 | 4 | 1 | 0 |
| L Rollouts | 4 | 0 | 3 | 1 |
| M Drift | 2 | 0 | 1 | 1 |
| N Audit | 2 | 1 | 1 | 0 |
| O Account, organization | 3 | 3 | 0 | 0 |
| P Operations | 6 | 3 | 2 | 1 |
| Q Runner lifecycle | 8 | 5 | 3 | 0 |
| **Total** | **96** | **61** | **30** | **5** |

A1, I1 and J2 are counted by their first verdict (`full`); their `partial` halves are listed in the gaps.

---

## Gaps (for step 2)

### P1 — core deploy / state / access / security, not covered end to end

1. **C5 Refresh-only run** (`none`). http e2e in `TestWorkspaceFlow`: `action=refresh` → claim plan → finish with changes → `needs_confirmation` even with auto-apply → confirm → apply writes state → `applied`.
2. **C2 Auto-apply** (store only). http e2e: auto-apply on, `action=apply` plan finishes with changes → claimable apply without confirm → `applied`; a plan without changes → `no_changes`.
3. **C10 / C9 Cancel end to end**. http e2e: (a) running: cancel → heartbeat `StatusCancel` → state write under the lock still accepted → finish `canceled` → run `canceled`, lock released, intermediate state kept as current; (b) queued: cancel → `canceled`, workspace free, artifacts gone.
4. **D4 Config restore through HTTP**. http e2e: configure a template to a new version → plan → discard (and: finish `failed` in plan) → Templates tab / job shows the previous version and inputs; added template discarded → gone.
5. **G3 / G4 Variable precedence and VCS-linked exceptions in the claimed job**. http e2e: global set, project set, workspace set, workspace var with the same key → job `Env` has the right winner; link the workspace to VCS (`POST /workspaces/{name}/vcs`) → job has no global/project set vars; change branch / turn on PR plans → workspace-attached set detached.
6. **Q4 Lease loss end to end**. http/app e2e: claim + ack, expire the lease, `ReapLeases` → run `errored` with "runner stopped responding" on the run page, state lock released, next log/finish → 409, artifacts deleted.
7. **F4 CLI force-unlock**. http e2e (go-tfe): user A locks, user B `ForceUnlock` → released; `ForceUnlock` on a run's/rollback's lock → refused (`ErrWorkspaceLockedByRun`).
8. **Real runner against the real server** (cross-cutting, `none` in `go test`). runner e2e: start `httptest.NewServer(srv.Handler())` and `runner.New(...).Execute` a real Terraform plan → confirm → apply with `terraform_data` (Terraform is already in CI for runner tests). Overlaps step 3 (`just smoke`).

### P2 — secondary flows (15)

1. D3 Remove a template: destroy confirmed and applied through the runner API; template gone, outputs-only cleanup reported — http e2e.
2. M1/M2 Drift: `action=assess` through HTTP; `App.QueueDriftChecks`; drift flag cleared by the next apply — http e2e + app.
3. L1–L3 Rollout through HTTP with real workspaces: start → `TickRollouts` → pause on discard → `POST …/rollout/resume` / `stop` — http e2e.
4. L4 Rollout audit entries (one per workspace, admin's name) — app.
5. H5 Make another project the default; new workspace without a project lands there (UI and cloud API) — http e2e.
6. G2 Global variable set (`POST /variable-sets/{set}` global flag), delete set, delete set variable — http e2e.
7. I4 Delete a VCS connection through HTTP: linked workspaces unlinked — http e2e.
8. I6–I8 `App.PollVCSRuns` (one poll per branch, errors, paused connection skipped); manual run of a tag-based workspace plans the newest tag — app.
9. B7 Execution mode switched through Settings › General: UI run → 409, CLI lock allowed; back to remote — http e2e.
10. E5 `App.ReleaseOrphanedLocks`: lock of a lease-less run released, claim woken — app.
11. N2 Audit page names the first broken entry after a tampered row (use `SECRET_KEY` mismatch, the table refuses UPDATE) — http e2e.
12. P2 `rotate-key` CLI path: env checks, audit entry, "value changed meanwhile" failure — app (extract or call `RotateKey` with a concurrent change).
13. A2 Logout ends the session (cookie cleared, next request redirected) — http e2e.
14. Runner token refused by the registry (documented: "the runner token can't use the registry") — add a principal to `onRegistry` in `TestAccessMatrix`.
15. Q8 The server puts sensitive variable values and sensitive inputs into the claimed job's `Secrets` (the runner's masking is tested, the list it masks isn't) — http e2e (claim, check `job.Secrets`).

### P3 — nice to have (9)

1. I1 Azure DevOps through the HTTP flows (connect, link, catalog write) — http e2e with a fake ADO.
2. K4 Delete a template in PR mode opens its own PR — http e2e.
3. P3 `opsolete token` CLI (stdout only, audited) — small test of `createToken` or an app helper.
4. P4 `/readyz` 503 without DB; `METRICS_ADDR` listener — http.
5. C12 Live SSE woken by a new chunk while the run is active — http.
6. C13 Organization currency reaching the job — http e2e.
7. Q8 256 KB log post limit → 413 — http.
8. A1 `serve()` startup refusals (short `RUNNER_TOKEN`, unusable `STATE_STORE`) — needs `serve` split for testability.
9. Q1 Runner refuses to start without child subreaper — runner (Linux).

---

## Docs vs implementation

### Documented, but not (or no longer) what the code does

- `usage/terraform-cli.md`: "An existing one needs operate access (read access is a 404 for the state…)". A reader gets **403** on `current-state-version`, `…-outputs`, `download`, `lock` (`http/access_test.go:TestAccessMatrix` cloud rows: reader = 403); a 404 is for no access at all.
- `operations/authentication.md`: "**Users** can create workspaces from templates…". Since the November 2026 decision they need `operate` on a project (the default one too); only `projects.md`/`workspaces.md` say so.
- `usage/variables.md` "Upgrading from runner pools" (queued runs errored by the migration): 1.0 starts from one fresh schema; nothing implements it.
- `usage/projects.md` "Existing workspaces were put into a project named `default` when projects arrived": 0.x migration wording; 1.0 creates `default` in the schema (unverified wording only).
- `catalog/vcs-providers.md` "Connections that limited repositories by pattern before this setting existed keep the workspaces already linked through them": no repository pattern exists in the 1.0 schema.
- `catalog/module-registry.md` "Versions uploaded before docs existed show a notice": a 0.x case; with a fresh 1.0 schema every version has docs (the notice in `views/module.html` is unreachable, unverified).
- `operations/authentication.md` "Workspaces created before login existed have no creator": 0.x wording (unverified whether creator-less workspaces still arise, e.g. via VCS or store-level creation).
- `operations/endpoints.md`: `POST /workspaces/{name}/runs` lists `action=apply|destroy|refresh|assess`; the code also takes `plan`. The rollout form also needs `source` and `ref` (stale version → 409), not only `project`, `batch`. `/workspaces/{name}` is described as "Overview, runs, outputs, resources, state versions"; runs and states are separate routes (`/runs`, `/states`).
- `getting-started.md`: `just smoke` "create a workspace end-to-end" — it only creates and lists one; no runner, no plan.
- `usage/workspaces.md` "Rename a workspace from its page": it is in Settings › General.

### Implemented, but not documented

- **UI workspace lock** (Settings › Locking: lock with a reason ≤ 300 chars, unlock by the holder, force unlock by any operator; runs wait, drift checks still run). Only a passing mention in the `STALE_LOCK_TIMEOUT` row of `configuration.md`.
- **Force delete** of a workspace whose state still manages resources (`force=1`, hidden field); `workspaces.md` says deletion needs an empty state.
- **Removing a template with nothing deployed** removes it at once, without a run.
- **Speculative plan by hand** (`action=plan`, "Plan only", status `planned`): not in `runs.md`'s kinds of runs or its status list (`planned`, `drifted` missing there); `state.md` mentions a "Plan only" run.
- **Execution mode** (remote/local) switch in Settings › General is listed but its effect on the UI (runs refused) is not explained in `workspaces.md`.
- **Endpoints missing from `endpoints.md`**: `/projects…`, `/variable-sets…`, `/catalog…`, `/organization`, `/audit`, `/account…`, `/templates/new`, `/templates/{id}/edit|delete`, `/registry/{ns}/{name}/{system}/{version}[/docs.md]`, `…/sync|unlink|delete`, `/vcs/{id}/token|delete`, `/workspaces/{name}/lock|project|variables|states|settings/{page}|deploy|vcs/edit|access`, `…/runs/{id}/logs/stream`, `/static/…`, `/.well-known/terraform.json`, `/v1/archives/…`, `/api/v2/ping`.
- **Runner API log post limit** of 256 KB per request (413).
