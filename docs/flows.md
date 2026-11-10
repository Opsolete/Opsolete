# Flow inventory (1.0, Phase 8 step 1)

Every user flow of Opsolete, the tests that cover it, and the gaps. Not published (outside `docs/content`).

**Verdicts.** `full`: the flow end to end through the real HTTP stack (`Server.Handler()`, real Postgres; the
runner's calls made by the test against the real runner API), or through the real runner (`runner.Execute`
against `fakeServer`). For background work with no HTTP surface (scheduler, notifier, retention), `full` means
the real component against real Postgres. `partial`: pieces tested at app/storage/package level, or steps or
failure paths missing (named). `none`: no test.

**Cross-cutting fact.** HTTP tests play the runner (`claim`/`ack`/`finish` by hand; `http/flows_test.go:fakeRunner`
for the flow tests), runner tests use `runner/helpers_test.go:fakeServer`. One test runs both halves together:
`e2e/e2e_test.go:TestRealRunner` (real server on a test database, real `runner.Runner`, real Terraform, the
greeting module from the server's own registry; skipped without `terraform` on PATH). The dev task `just seed`
and the NixOS VM test (`nix/test.nix`, boots both, runs no job) remain outside `go test`; `just smoke` only
creates and lists a workspace.

Test paths are relative to `internal/`, except `cmd/opsolete/…` (from the repository root).

---

## A. Platform, login and access

**A1. First start + first admin login** — `full` (login), `partial` (startup)
- Actor: operator deploying; first admin (in `ADMIN_EMAILS`/`ADMIN_GROUPS`).
- Pre: Postgres, `SECRET_KEY` ≥ 32, `RUNNER_TOKEN` ≥ 32 and `STATE_STORE` (with deployments), OIDC or `AUTH_DEV_USER`.
- Steps: start `opsolete` (migrations, signing key, state store probe, catalog load, `CheckTemplates`) → open UI → redirected to `/auth/login` → IdP → `/auth/callback` → session cookie.
- Outcome: user row keyed by issuer+sub, groups recorded, role admin/user re-derived every request; sign-in audited.
- Failures: bad config (startup error); forged state/nonce, `email_verified:false`, email of another subject → 4xx; open redirect blocked; `preferred_username` never matches `ADMIN_EMAILS`.
- Coverage: `auth/auth_test.go:TestOIDCLogin`, `TestOIDCLoginRejects`, `TestSessionCookie`, `TestLocalPath`; `storage/storage_test.go:TestUpsertUserSubject`; `http/server_test.go:TestLoginRequired`; `config/config_test.go:TestLoad`; `http/server_test.go:TestFeatureSwitches` (CheckTemplates). Missing: `serve()` startup checks (RUNNER_TOKEN length, STATE_STORE probe, `AUTH_DEV_USER` warning) untested.

**A2. Logout** — `full`
- Actor: any user. Steps: `POST /auth/logout`; **Account › Sessions › Log out everywhere** (`POST /account/sessions/revoke`); admin: audit log entry › **End this user's sessions** (`POST /users/{id}/sessions/revoke`).
- Outcome: the session row is deleted (IdP session stays), so a copied cookie stops working too; log out everywhere / the admin action delete all of the user's sessions (log out everywhere keeps `terraform login` tokens; the admin action revokes them too). Sessions are server-side (`sessions`, token hash only, 12 h, `sessionTTL`); the cookie is an opaque 256-bit token; role re-checked per request; expired rows deleted by the hourly retention job. Dev user mode has no sessions.
- Coverage: `http/server_test.go:TestLogout` (login creates a row, cookie holds no identity, sessions page marks this browser, copied cookie dead after logout while another browser stays, log out everywhere, admin ends a user's sessions from the audit log, expired session → login); `auth/auth_test.go:TestSessionCookie`, `TestOIDCLogin` (role follows ADMIN_GROUPS per request), `TestDevUser`; `app/retention_test.go:TestRetentionSessions`; `http/access_test.go:TestAccessMatrix` (`POST /auth/logout`, `POST /account/sessions/revoke`, `POST /users/{id}/sessions/revoke`); `http/forms_test.go` no-input list.

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

**B7. Execution mode (remote ↔ local)** — `full`
- Local: runs refused (409), CLI may lock/write state; remote: CLI lock/state upload refused.
- Coverage: `http/flows_test.go:TestExecutionMode` (switched in Settings › General: remote refuses the CLI lock; local refuses a UI run (409, nothing claimable), the CLI locks and unlocks; back on remote runs again, the CLI refused); `app/runs_test.go:TestStartRun`; `http/cloud_test.go:TestCloudLoginAndState`.

**B8. Workspace pages (overview, runs, states, variables, outputs, resources, templates)** — `full`
- Coverage: `http/runs_test.go:TestWorkspaceFlow/workspace_page_shows_resources,_outputs_and_templates`, `…/settings_pages_and_workspace_tabs_render`; `http/workspacetemplates_test.go:TestGroupResources`; `http/render_test.go:TestPageModels`.

---

## C. Runs

**C1. Plan → confirm → apply → outputs** — `full`
- Actor: operator; runner. Pre: workspace with configuration.
- Steps: `POST /workspaces/{name}/runs action=apply` (or create/configure) → runner claims plan, acks, locks state, uploads plan + lock file, finishes with summary/cost → `needs_confirmation` → `POST …/runs/{id}/confirm` → runner claims apply (same state password), downloads artifacts, writes state under lock, finishes with outputs + resources.
- Outcome: `applied`; state version indexed; sensitive outputs `(sensitive)`; artifacts and intermediate versions deleted; credential dead after the run.
- Failures: stranger confirm → 404; second run while one waits → 409; no claim while waiting; state access while waiting → 403; Terraform refuses a stale plan at apply.
- Coverage: `http/runs_test.go:TestWorkspaceFlow` (steps "plan job carries…" through "a finished apply indexes its state…", "plan page shows the changes and cost"); `e2e/e2e_test.go:TestRealRunner` (real runner and Terraform against the real server: plan, confirm, apply, outputs, indexed state, log; destroy); `runner/runner_test.go:TestPlanThenApply` (real Terraform: plan, apply, stale plan refused); `storage/jobs_test.go:TestRunLifecycle`; `app/runs_test.go:TestRunEndDeletesArtifacts`.

**C2. Auto-apply** — `full`
- Workspace setting; an apply plan with changes goes straight to `apply_queued`. Destroys, refresh-only and rollout plans still ask.
- Coverage: `http/flows_test.go:TestAutoApply` (apply plan → `apply_queued` without confirmation → applied; no changes → `no_changes`; plan-only → `planned`; destroy and rollout plans ask); `http/flows_test.go:TestRefreshOnlyRun` (refresh-only asks); `storage/jobs_test.go:TestAutoApplyAndDiscard`; `http/runs_test.go:TestWorkspaceFlow/a_destroy_plan_asks_even_with_auto-apply…`.

**C3. Discard** — `full`
- `POST …/runs/{id}/discard` → `discarded`, plan artifacts deleted, workspace free.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_destroy_plan_asks_even_with_auto-apply;_discarding_deletes_it`; `app/runs_test.go:TestRunEndDeletesArtifacts`; `storage/jobs_test.go:TestAutoApplyAndDiscard`.

**C4. Destroy** — `full`
- Settings › Destruction: `action=destroy` → plan -destroy → always asks → confirm → apply → workspace shows `destroyed`, no resources.
- Failures: stranger 404; reader 403.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_confirmed_destroy_empties_the_state`; `runner/runner_test.go:TestPlanThenApply` (destroy phase); `http/variables_test.go:TestTeamsVariablesRenameCancel` (reader 403).

**C5. Refresh-only run** — `full`
- `action=refresh` → `plan -refresh-only` → always asks (even with auto-apply) → apply updates state only.
- Failures: discarded → nothing written, plan deleted; no changes → `no_changes`; refresh + replace → 422.
- Coverage: `http/flows_test.go:TestRefreshOnlyRun`; `http/runs_test.go:TestWorkspaceFlow/replace_sends_only_addresses…` (refresh + replace → 422). The runner's `-refresh-only` flag: `runner/runner_test.go:TestPlanThenApply` (assess).

**C6. Replace resources** — `full`
- Tick resources → `action=apply replace=<addr>` → job carries `Replace` → plan shows replace.
- Failures: unknown address 422; replace with non-apply action 422.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/replace_sends_only_addresses…`; `runner/runner_test.go:TestPlanThenApply` (replace); `app/runs_test.go:TestStartRun`.

**C7. Plan only (speculative, by hand)** — `full`
- `action=plan` → plan → ends `planned`, never applied.
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_plan-only_run_with_changes_ends_as_planned`.

**C8. One active run per workspace** — `full`
- Coverage: `http/runs_test.go:TestWorkspaceFlow/a_plan_waiting_for_confirmation_holds_the_workspace…`; `storage/jobs_test.go:TestRunLifecycle`, `TestConcurrentTriggers`; `app/runs_test.go:TestStartRun`.

**C9. Cancel a queued run** — `full`
- `POST …/cancel` on a queued run → `canceled` at once, artifacts deleted, configuration change undone.
- Coverage: `http/flows_test.go:TestCancelRun/a_queued_run_is_canceled_at_once…` (confirmed apply waiting behind a UI lock: canceled, lock and configuration kept, plan deleted, second cancel 409); `http/flows_test.go:TestConfigRestore/a_canceled_queued_plan_puts_it_back`; `app/runs_test.go:TestRunEndDeletesArtifacts`, `storage/jobs_test.go:TestCancel`, `storage/runconfig_test.go:TestRunConfigRestore`.

**C10. Cancel a running run (graceful)** — `full`
- Operator cancels → next heartbeat/log post answers `StatusCancel` (lease kept) → runner SIGINTs Terraform, which saves state → runner finishes `canceled` → run `canceled`, lock released.
- Coverage: `http/flows_test.go:TestCancelRun/a_running_apply_stops_when_its_runner_hears_the_cancel…` (page: "Cancel requested…", Force cancel; heartbeat and log 202; state write under the run's lock accepted and kept as current; finish `canceled`; lock released; message on the run page; late log 409); `http/variables_test.go:TestTeamsVariablesRenameCancel/cancel_reaches_the_runner_on_its_heartbeat…`; `storage/jobs_test.go:TestCancel`; `runner/terraform_test.go:TestCancelSavesState`, `TestInterrupt/cancel_*` (the runner half).

**C11. Force cancel** — `full`
- After a pending cancel, `force=1` → run `canceled`, runner fenced off (409), state lock and password dead.
- Failures: cancel of a finished run → 409.
- Coverage: `http/flows_test.go:TestCancelRun/force_cancel_ends_a_running_run_at_once…` (lock released; heartbeat, log, finish 409; state write 403, nothing saved; message on the run page); `http/variables_test.go:TestTeamsVariablesRenameCancel/cancel_reaches…,_force_cancel_cuts_it_off`; `storage/jobs_test.go:TestCancel`, `TestStateWriteFenced`; `app/state_test.go:TestStateRunCredential` (credential dead).

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

**D3. Remove a template** — `full`
- Resources deployed → destroy run targeted at `module.<name>`; on apply the template goes and an outputs-only cleanup plan (`-refresh=false`) drops its outputs. Last one → plain destroy. Nothing deployed → removed at once, no run.
- Failures: removing twice → 404; active run → 409.
- Coverage: `e2e/e2e_test.go:TestRealRunner/removing_a_deployed_template…` (real runner and Terraform: targeted destroy planned, the template kept until applied, removing another one meanwhile 409, confirmed, applied: "1 destroyed" and "terraform apply (outputs only)" in the log, template, resources and its outputs gone, the other's kept); `http/workspacetemplates_test.go:TestWorkspaceTemplates` (destroy job + target, removing twice 404, immediate removal); `runner/assemble_test.go:TestAddedTemplates`.

**D4. Configuration restored when a run ends before its apply** — `full`
- Adding/configuring/rollout run discarded, canceled or errored while planning → previous version and inputs back (added template removed); once apply started the new one stays; a later change isn't overwritten.
- Coverage: `http/flows_test.go:TestConfigRestore` (configure to a new version and inputs, two catalogs on one database: discarded, failed plan, canceled while queued → old version and inputs back on the Templates tab with "update: v2"; added template discarded → gone; apply started then failed → new version kept); `storage/runconfig_test.go:TestRunConfigRestore` (a later change isn't overwritten); `app/rollouts_test.go:TestTickRollout` (discard restores); `storage/rollout_test.go:TestUpgradeTemplates`.

**D5. Sensitive template inputs** — `full`
- Password field, sealed bound to workspace+module, `(sensitive)` on pages, `(redacted)` in audit, kept when left empty, clear in the job; a moved sealed value fails the job.
- Coverage: `http/workspacetemplates_test.go:TestSensitiveInputs` (also in the job's `Secrets`); `e2e/e2e_test.go:TestRealRunner/a_sensitive_input_reaches_Terraform…` (real runner: plan and apply logs show `***`, no page shows it); `app/sealing_test.go:TestVariableAD`; `app/inputs_test.go:TestSealInputsKeepsRejectedError`; `registry/registry_test.go:TestResolveSensitive`.

**D6. Form rules (validation, help, show_if, defaults)** — `full`
- Lists (`list(object({...}))` → rows of fields): fields from the module's type (`optional()` defaults), rows posted as `<list>.<row>.<field>` in row order, empty rows dropped, each field checked like an input ("Row 2, LUN: …"), typed list to Terraform, rows on the edit form and the workspace page; the builder keeps a list's fields, with their labels. Coverage: `templates/list_test.go:TestListInput`; `registry/adopt_tree_test.go:TestListInputs`, `TestAdoptableTree`; `http/listinputs_test.go:TestListInputForm`; `http/builder_test.go:TestBuilderListInput`. Missing: the row JavaScript (+ Add row, Remove) in a browser.
- Coverage: `http/templates_test.go:TestInvalidSubmitRerendersForm`; `http/render_test.go:TestRenderShowIf`; `templates/templates_test.go:TestShowIf`, `TestShowIfBool`, `TestValidateEdgeCases`, `TestLoadCatalogAndValidate`; `templates/json_test.go:TestSchema`, `TestJSONRoundTrip`.

**D7. Import existing resources** — `partial`
- Actor: operate. Steps: Import… → optional resource group → Scan (run action `scan`: runner `terraform query`, fit plan into a throwaway state) → scan page: found resources with template, related resources, fit (fits / differs / doesn't fit / incomplete / managed) → Import → the template's add form filled in → plan with import blocks into `module.<name>` → confirm (always, also with auto-apply) → imported. Design: `docs/resource-import.md`.
- Failures: invalid resource group name → 422; no registry template can import → 422; candidate of another template → 404; a resource a workspace manages (index) → 422; plan replaces or deletes, or imports a resource a state has (as the plan read it) → run errored, template removed again.
- Coverage: `http/scan_test.go:TestImportFlow` (import page, scan job with the derived entries, results page, filled form without IDs, imports and placeholder in the job, managed after apply, refused again); `registry/adopt_test.go:TestAdoptable` (the example VM module), `TestAdoptableSkips`, `adopt_tree_test.go:TestAdoptableTree` (the composed VM module and its registry building blocks; optional parts left for the form); `runner/scan_test.go:TestCandidates`, `TestFit` (real Terraform fit plan), `TestFitPlan`, `TestParseQuery` (unsupported list types), `TestQueryFile` (resource group), `TestPath`, `TestUses`; `runner/assemble_test.go:TestImports` (real Terraform import), `TestSummarizeImport`; `storage/runconfig_test.go:TestAdoption` (imports in both phases, auto-apply waits, replace → errored, managed → errored). Missing: `terraform query` and the inventory plan themselves (need cloud credentials; checked live, see the design doc), the 50-candidate cap.

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

**E5. Orphaned locks released** — `full`
- Leader job every 15 s: a run's lock without a lease, a rollback's lock older than `STALE_LOCK_TIMEOUT`; CLI/UI locks stay.
- Coverage: `app/state_test.go:TestReleaseOrphanedLocks` (`App.ReleaseOrphanedLocks`: a queued run's lost lock and an old rollback's released and logged, a new rollback's and a CLI lock kept, claimers woken, the run behind its lock claimed; a working run's lock kept); `storage/jobs_test.go:TestReleaseOrphanedLocks`.

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
- Failures: save without lock 409; second lock 409 (`ErrWorkspaceLocked`); unlock of another user's lock → `ErrWorkspaceLockedByUser`; of a run's lock → `ErrWorkspaceLockedByRun`; upload-URL flow → 422 fallback to inline; outputs list names only; outputs containing a sensitive variable's value stored `(sensitive)` (`app/state_test.go:TestCLIStateMasksSecrets`); reader → 403; stranger → 404.
- Coverage: `http/cloud_gotfe_test.go:TestCloudGoTFE`; `http/cloud_test.go:TestCloudLoginAndState`; `storage/jobs_test.go:TestCloudStateWriteFenced`.

**F4. `terraform force-unlock`** — `full`
- Releases a CLI lock (any operator); never a run's or rollback's: 423 "workspace is locked by Run…" (not 409, which go-tfe reports as "workspace already unlocked"), the lock kept.
- Failures: read access refused.
- Coverage: `http/cloud_gotfe_test.go:TestCLIForceUnlock` (go-tfe `ForceUnlock`: another user's CLI lock released, rollback lock and a planning run's lock refused and kept, the run's lock gone when it ends); `storage/jobs_test.go:TestCloudStateWriteFenced` (a force-unlock racing an upload).

**F5. Remote-execution workspace refuses the CLI** — `full`
- Coverage: `http/cloud_test.go:TestCloudLoginAndState` (lock and state upload → 409 "remote execution"; run on a local workspace → `ErrLocalExecution`).

---

## G. Variables and variable sets

**G1. Workspace variables** — `full`
- Operate: add env/terraform, sensitive write-only; reserved env names refused (`PATH`, `TF_*`, `GIT_*`, `OPS_*` but `OPS_DEBUG`); delete. Values reach the claimed job.
- Coverage: `http/variables_test.go:TestTeamsVariablesRenameCancel/operators_set_workspace_variables…`, `…/the_runner's_job_carries…`; `app/variables_test.go:TestVariableAccess`; `http/audit_test.go:TestAuditLogRecordsChanges` (value redacted); `runner/runner_test.go:TestJobEnv`, `TestCompactWarnings` (`OPS_DEBUG`).

**G2. Variable sets (admin)** — `full`
- Create (optionally for a project or workspace), add/delete variables, attach to workspaces and projects, global flag, delete set.
- Coverage: `http/flows_test.go:TestVariableSetLifecycle` (unattached: in no job; global on/off through `POST /variable-sets/{set}`; a deleted set variable and a deleted set leave the next job and the pages); `http/variables_test.go:TestTeamsVariablesRenameCancel` (create, vars, attach ws, non-admin 403, unknown ws 422); `http/projects_test.go:TestProjectPages`, `TestAttachProjectsParsesForm`; `storage/jobs_test.go:TestRunVariables`.

**G3. Variable precedence** — `full`
- template inputs > workspace vars > sets on the workspace > sets on its project > global sets; same kind: first by name.
- Coverage: `http/flows_test.go:TestJobVariables/workspace_variables_win…` (every tier, env and terraform, in the claimed job; sensitive values in `Env` and `Secrets`, on no page); `storage/jobs_test.go:TestRunVariables`; `app/variables_test.go:TestWithWorkspaceVars` (template inputs win; registry modules only); `http/variables_test.go:TestTeamsVariablesRenameCancel`.

**G4. VCS-linked workspaces and sets (security rule)** — `full`
- Linked workspaces get no global or project sets; changing repo/branch, switching to/changing tag triggers, or turning on PR plans detaches the workspace's own sets.
- Coverage: `http/flows_test.go:TestJobVariables` (link → no project/global sets, own sets detached; re-attached → in the job; saved unchanged → kept; PR plans on → detached; variables page follows); `storage/jobs_test.go:TestVCSWorkspaceVariableSets`, `TestRunVariables/a_VCS-linked_workspace_gets_only_the_sets_attached_to_it` (branch and tag changes).

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

**H5. Make another project the default** — `full`
- `POST /projects/{project}/settings` with default → `organization.default_project_id`; new workspaces and the cloud API without a project land there.
- Coverage: `http/flows_test.go:TestDefaultProject` (non-admin 403; blank, template and cloud API workspaces without a project land in the new default; it can't be deleted (409), the old one can).

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

**I4. Delete a connection** — `full`
- Unlinks its workspaces; refused while a linked workspace has an active run or the catalog uses it.
- Coverage: `http/flows_test.go:TestDeleteVCSConnection` (409 with the reason during a linked workspace's run, links kept; deleted: gone from `/vcs`, both workspaces unlinked, no repository shown, a run 409 (nothing to plan)); `app/vcs_test.go:TestVCSConnectionsAdminOnly`, `app/catalog_test.go:TestDeleteVCSConnectionDuringRun`; `http/catalog_test.go:TestTemplateCatalogInGit/the_connection_can't_be_deleted_under_the_catalog…`.

**I5. Link a workspace to a branch; change source; unlink** — `full`
- Operate; at create (`/workspaces/new?type=vcs`) or Version control › Change source; branch checked; runs plan the pinned commit via a signed archive; relink during a run 409; unlink (`type=cli`).
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces` ("a linked workspace plans its branch head…", "a linked workspace shows its source…", unlink); `app/vcs_test.go:TestLinkVCSDuringRun`, `TestVCSArchiveSignature`; `app/app_test.go:TestCreateEmptyWorkspaceAtomic`.

**I6. Push → run** — `full`
- Leader polls each branch once a minute; each new commit without a run queues an apply run of it; only the newest if several landed.
- Coverage: `app/vcsruns_test.go:TestPollVCSRuns` (`App.PollVCSRuns`: one request per branch for two workspaces, a failing repository logged without keeping the others from their runs, a connection the provider asked to wait skipped, a commit runs once); `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/commits_queue_apply_runs…`; `storage/jobs_test.go:TestQueueVCSRun`, `TestConcurrentTriggers`.

**I7. Pull request → speculative plan** — `full`
- Opt-in; each new head commit of an open PR into the branch gets a plan-only run; forks skipped; never applied; no shared state.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/commits_queue_apply_runs,_pull_requests_speculative_plans`; `storage/jobs_test.go:TestQueueVCSRun` (ends `planned`); `vcs/vcs_test.go:TestProviders` (forks left out); `http/state_test.go:TestRemoteStateSharing`; the poll loop: `app/vcsruns_test.go:TestPollVCSRuns`.

**I8. Tag-based triggers** — `full`
- Format (semantic, v-prefix, suffix, custom regex); newest matching tag at switch time is the baseline and never runs; later tags queue apply runs; manual runs plan the newest tag; PR plans off.
- Coverage: `http/workspaceconfig_test.go:TestBlankAndVCSWorkspaces/tag_triggers_run_only_tags_pushed_after…`; `app/vcsruns_test.go:TestPollVCSRuns` (the baseline doesn't run, a newer tag does through the poll loop, a run by hand plans the newest matching tag, not a prerelease), `TestLatestTag`.

**I9. Polling cost control (ETags, pauses, rate limits)** — `partial`
- Coverage: `vcs/vcs_test.go:TestConditionalRequestsAndPause`, `TestRateLimitedResponsePauses` (package level).

---

## J. Module registry

**J1. Upload a module** — `full`
- Admin, with Settings › Organization › Module uploads on (or `TEMPLATE_EDITOR`); `.tar.gz`, semver; immutable; docs generated.
- Failures: uploads off / non-admin 403; re-upload 422 "immutable"; bad name 422.
- Coverage: `http/registry_test.go:TestRegistry`; `http/catalog_test.go:TestModuleUploadsSetting`; `http/builder_test.go:TestEditorIsAdminOnly`; `app/catalog_test.go:TestUploadModuleRefused`; `registry/registry_test.go:TestInspectAndInputs`, `TestInspectRejects`.

**J2. Publish from VCS tags; sync; unlink** — `full` (manual sync), `partial` (scheduled)
- Wizard (connection → repo → name/system from `terraform-<system>-<name>`, optional folder, optional tag prefix); semver tags become versions (with a prefix: only tags `<prefix><semver>`, e.g. release-please's `data-disk-v1.0.0`); folder missing in older tags skipped; Sync now; unlink keeps versions.
- Failures: folder escaping the repo 422; invalid tag prefix 422; missing folder → sync error shown; unlink twice 404.
- Coverage: `http/vcs_test.go:TestVCSPublishAndUpdate`; `app/vcssync_test.go:TestNoFolderCache` (the leader job `PollVCSModules`, cache only), `TestTagPrefix` (modules released separately from one repo).

**J3. Module docs pages** — `full`
- Coverage: `http/registry_test.go:TestRegistry` (HTML, README escaped, `docs.md`, unknown version 404).

**J4. Delete a module** — `full`
- Refused while a template uses it (409); removes every version; 404 twice.
- Coverage: `http/registry_test.go:TestRegistry`.

**J5. `terraform init` against the registry** — `full`
- Registry token (`opsolete token`) or `terraform login` token; versions → download (`X-Terraform-Get` signed 10-min URL) → archive; tampered signature 403.
- Runs: a job's registry token (its run's, read-only, only while a runner works on it) lets a template's module call other registry modules; the runner writes it as a CLI `credentials` block.
- Coverage: `http/registry_test.go:TestRegistry`; `http/access_test.go:TestAccessMatrix` (registry rows: user and registry tokens allowed, the runner token refused); `registry/registry_test.go:TestAddressAndSigning`; `http/scan_test.go:TestRunRegistryToken` (valid while working, not after, not tampered); `runner/scan_test.go:TestCLIConfig`; `e2e/e2e_test.go:TestRealRunner/a_template's_module_calls_another_registry_module…` (real runner and Terraform, over plain HTTP: the nested module downloads with the run's token).

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

**L1. Start a rollout** — `full`
- Admin; Roll out on the template; scope (project or all), batch size (0 = all); must name the version the page showed.
- Failures: stale version 409; negative batch 422; non-admin 403; nothing to do → `done` at once.
- Coverage: `http/flows_test.go:TestRollout` (four workspaces on v1, batch 2: the first two plans queued at once, the job on v2; non-admin 403); `http/rollout_test.go:TestStartRollout`, `TestRenderRollout`; `http/forms_test.go:TestFormsRerender` (batch); `app/rollouts_test.go:TestRolloutsAdminOnly`.

**L2. Rollout progress (batches, busy, needs input, local, done)** — `full`
- Leader tick every 15 s; plans always ask even on auto-apply; busy retried; needs-input/local skipped.
- Coverage: `http/flows_test.go:TestRollout` (`App.TickRollouts` on due rollouts: a full batch queues nothing more, the local workspace is left, done with "1 use(s) need an update by hand", workspaces on v2); `http/flows_test.go:TestAutoApply` (rollout plans ask); `app/rollouts_test.go:TestRolloutPlan`, `TestTickRollout` (busy, needs input); `storage/rollout_test.go:TestUpgradeTemplates`.

**L3. Pause on failure, resume, stop** — `full`
- Own run errored/discarded/canceled → paused; Resume retries; Stop ends it; template moved to another version → stopped.
- Coverage: `http/flows_test.go:TestRollout` (a discarded plan pauses it, named on the page, the workspace back on v1; `POST …/rollout/resume` retries it with the next one; pause/resume by hand; stop leaves its plan queued, a second stop and a pause of a done rollout 409); `app/rollouts_test.go:TestTickRollout` (stopped on version change); `storage/rollout_test.go:TestUpgradeTemplates`; `http/rollout_test.go:TestStartRollout`.

**L4. Rollout audit entries** — `full`
- One `template rollout` entry per upgraded workspace, in the admin's name.
- Coverage: `http/flows_test.go:TestRollout/one_audit_entry_per_upgraded_workspace…` (every batch, the retried workspace twice, from → to).

---

## M. Drift detection

**M1. Scheduled drift checks** — `full`
- Settings › Health on → leader queues a refresh-only assess run once a day per workspace that manages resources; never applied; not in the runs list; banner + *drifted* tag; runs in a locked workspace.
- Coverage: `http/flows_test.go:TestDriftChecks` (`App.QueueDriftChecks`: nothing while off; on: an assess job, drifted, banner and list tag, not in the runs list, never applied, not again within the day; cleared by a check without drift and by an apply); `http/runs_test.go:TestWorkspaceFlow/drift_check_is_scheduled…`; `http/settings_test.go:TestWorkspaceLock`; `runner/runner_test.go:TestPlanThenApply` (assess plan).

**M2. Check for drift now** — `full`
- `POST /workspaces/{name}/runs action=assess` (Overview, "Check for drift").
- Coverage: `http/flows_test.go:TestDriftChecks` (with and without drift; never applied).

---

## N. Audit log

**N1. View the audit log (changes, sign-ins and refusals)** — `full`
- Admin; every state-changing request, sign-ins, 401/403 incl. reads; principal, IP, form (secrets `(redacted)`, values capped); refusals on `?show=access`.
- Coverage: `http/audit_test.go:TestAuditLogRecordsChanges`, `TestAuditCapsForms`, `TestAuditRowSummary`; `auth/auth_test.go:TestOIDCLogin` (sign-in principal); `http/workspacetemplates_test.go:TestSensitiveInputs` (redaction).

**N2. Verify / tamper evidence** — `full`
- Append-only table; HMAC chain under `SECRET_KEY`; page re-verifies and names the first broken entry; `SECRET_KEY_PREVIOUS` verifies pre-rotation entries.
- Coverage: `http/audit_test.go:TestAuditLogTampered` (UPDATE and DELETE refused; an entry appended without `SECRET_KEY` is named on the page, valid entries after it notwithstanding); `storage/audit_test.go:TestAuditLogAppendOnly`, `TestAuditLogKeyRotation`; `http/audit_test.go:TestAuditLogRecordsChanges` ("intact").

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

**P2. Key rotation (`opsolete rotate-key`)** — `full`
- Restart with both keys → rotate (re-seal variables, VCS tokens, sensitive inputs, state files, plans; refuse with an active run; idempotent) → drop `SECRET_KEY_PREVIOUS`.
- Coverage: `cmd/opsolete/main_test.go:TestRotateKeyCLI` (refused without `SECRET_KEY_PREVIOUS`, with it equal or short, with an unusable `STATE_STORE`, none audited; a variable and a state store file re-sealed, audited with the count; again: 0); `app/rotate_test.go:TestRotateKey`, `TestRotateKeyRace` (a value changed meanwhile keeps the server's write and fails the rotation; a run started meanwhile fails it); `secrets/secrets_test.go:TestRotation`; `storage/audit_test.go:TestAuditLogKeyRotation`.

**P3. Registry token from the CLI (`opsolete token <name>`)** — `full`
- Prints the token on stdout only, audits `create token`.
- Coverage: `cmd/opsolete/main_test.go:TestCreateToken` (stdout is the token alone, it works as a registry token, the audit entry names it without the token).

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

**Q4. Lease loss (runner disappears; runner fenced off)** — `full`
- Server: no heartbeat for the lease TTL → run errored ("runner stopped responding"), lock released, late writes 409, artifacts deleted. Runner: a 409 interrupts Terraform and reports nothing.
- Coverage: `http/flows_test.go:TestLeaseLoss` (apply phase, lease aged in the DB, `App.ReapLeases`: errored, message on the run page, state lock released, plan deleted; heartbeat, log, finish, artifact 409; state write 403, nothing saved; workspace free); `storage/jobs_test.go:TestExpiredLeaseIsErrored`; `runner/terraform_test.go:TestLostLeaseStopsTerraform` (the runner half).

**Q5. Drain / scale down (first SIGTERM)** — `full`
- Stops claiming; a claim in flight completes, is acked and run; then the runner exits.
- Coverage: `runner/runner_test.go:TestDrainKeepsClaimedJob`. (The signal handler in `cmd/opsolete:runRunner` is untested.)

**Q6. Interrupts: timeout + grace, cancel, second SIGTERM** — `full`
- SIGINT, then kill after `OPS_JOB_GRACE`; messages name cause and outcome; no logs/heartbeats past the hard end.
- Coverage: `runner/terraform_test.go:TestInterrupt` (all causes × clean/killed, stall between commands); `runner/ship_test.go:TestShipperStopsAtHardEnd`; `config/config_test.go:TestLoadRunner`.

**Q7. Job isolation and cleanup** — `full` (Linux)
- Own temp dir and `HOME`; job env on top of runner env, `OPS_*` scrubbed; every leftover process killed after each job.
- Coverage: `runner/runner_test.go:TestJobHome`, `TestJobEnv`; `runner/isolate_linux_test.go:TestJobProcessesKilled` (Linux only; `just test-linux`).

**Q8. Log shipping, masking, caps** — `full`
- ~1 s batches cut at line ends; sensitive values and signed URLs masked; 10 MB per run; 256 KB per post.
- Masking rule in one place, `internal/mask` (secrets ≥ 4 non-space chars, multi-line ones also line by line, longest first): the log shows `***`; an output string containing a secret (also nested in lists/maps) is stored whole as `(sensitive)`: by the runner for `runs.outputs` (`Job.Secrets`), by `saveState` for `state_versions.outputs` (runs, CLI uploads, rollbacks; the workspace's sensitive variables and inputs via `workspaceMasker`; if they can't be opened every output is stored `(sensitive)`), and in state diffs for every leaf (outputs and attributes). The cloud outputs endpoint lists names only.
- Coverage: `e2e/e2e_test.go:TestRealRunner/a_sensitive_input_reaches_Terraform…` (the real runner masks a sensitive template input Terraform prints); `mask/mask_test.go:TestMasker`; `app/state_test.go:TestCLIStateMasksSecrets`; e2e also checks run page, Outputs tab, state diff, cloud outputs and the stored rows show `(sensitive)`, never the value; `runner/ship_test.go:TestShipperRedactsAndDrains` (signed URLs; `Job.Secrets` whole, per line, short ones kept); `runner/terraform_test.go:TestHeadBuffer`; `storage/jobs_test.go:TestRunLifecycle` (cap via `AppendLog` max); `runner/runner_test.go:TestPlanThenApply` (no leaks); `http/flows_test.go:TestJobVariables` (sensitive variables in `Job.Secrets`); `http/workspacetemplates_test.go:TestSensitiveInputs` (sensitive template inputs in `Job.Secrets`); `http/logs_test.go:TestLogPostLimit` (a post over 256 KB 413, nothing stored; one at the limit accepted).

---

## R. Environments and promotion

**R1. Set environment values on a project** — `full`
- Project › Environment values; admins and groups with operate on the project set/clear (`POST /projects/{project}/environment`, `…/clear`); readers see them; checked by the input's own rules, not empty.
- Failures: reader 403; unknown template/input or not per-environment 404; invalid value 422.
- Coverage: `http/environments_test.go:TestEnvironmentValuesPages` (set as admin, read-only for a reader); `http/access_test.go:TestAccessMatrix` (the three routes); `app/environments_test.go:TestEnvironmentValueAccess`; `templates/json_test.go` (`per_environment` not with sensitive/key, schema and loader agree); `http/builder_test.go` (checkbox).

**R2. Per-environment input at run time** — `partial`
- The project value replaces the workspace's own when a runner claims the job (the workspace's current project; moving switches it); hidden by `show_if` → not set; a value the template now rejects fails the job. Forms show it read-only ("Set by project X"), a posted value is ignored; the deploy form re-renders for another project; rollouts count it.
- Coverage: `app/environments_test.go:TestEnvironmentValuesInJobs` (`Store.ClaimJob` + `assembleJob`: precedence, move to another project, stored config never holds project values, restore), `TestEnvironmentValueShowIf`; `http/environments_test.go:TestEnvironmentValuesPages` (deploy/configure forms locked, posted value ignored, project switch re-renders and creates nothing); `app/rollouts_test.go:TestRolloutPlanEnvironment`; `app/promote_test.go:TestPromote` ("the project's value applies").
- Missing: the job content through the runner API or the real runner (app level only); a rejected project value failing the job.

**R3. Promote to an existing workspace, one run** — `full`
- Promote to… (read on the source, operate on the target); preview (added/updated/replaced/removed with resource counts/unchanged, per-environment source, sensitive kept or asked); pinned by a digest; one apply run with every change in config_changes, always waits for confirmation; discard/failed plan restores the target's templates incl. removed ones; audit `promote`.
- Failures: stale preview 409; target busy 409; nothing to change 409; to itself / local target refused; read-only target 403; invalid asked values 422.
- Coverage: `e2e/e2e_test.go:TestRealRunner` (promote steps: "nothing left to remove, one run; applied, the target runs exactly dev's templates"); `http/promote_test.go:TestPromotePages`; `app/promote_test.go:TestPromote` (preview, stale, exact copy, own size and password kept, nothing to change, auto-apply target waits, discard restores, audit), `TestPromoteAccess`; `storage/promote_test.go:TestPromote` (discarded / failed plan restore incl. a removed template, a taken name wins); `http/access_test.go:TestAccessMatrix`; `http/forms_test.go`; `http/render_test.go`.

**R4. Promote with a deployed template removed or replaced (two chained runs)** — `full`
- Preview says "runs twice"; run A destroys (`-target`) only the removed/replaced modules, always confirms, carries the promotion; A applied/no_changes → removal rows deleted, run B (apply, always confirms) queued, audit `promote apply queued`; A ending otherwise drops it ("promotion not continued"); B discarded → the post-A templates.
- Coverage: `e2e/e2e_test.go:TestRealRunner` (the removed module configures its own provider: destroy run with its block, then the apply run; discarded → back to post-destroy templates; re-promotion is one run); `storage/promote_test.go:TestPromoteChained` (incl. "promotion not continued" for failed/discarded/canceled, `promote apply queued` audit), `TestModuleIndex`; `app/promote_test.go:TestPromoteChained` (audit `chained`).

**R5. Promote to a new workspace** — `full`
- Name + a project the user may create workspaces in; the workspace is created by the promotion (remote execution) with one run that waits; sensitive and per-environment inputs without a value are asked for; discard leaves it without templates.
- Failures: name taken / invalid 422; project the user may not create in 422.
- Coverage: `http/promote_test.go:TestPromotePages` (picker, preview `?new=`, refused form re-rendered with field errors, run opened); `app/promote_test.go:TestPromote` ("a new target: per-environment and sensitive values asked for"), `TestPromoteAccess`; `storage/promote_test.go:TestPromote` ("a new workspace: created with the templates, removed again on discard", taken name).

---

## Counts

| Area | Flows | full | partial | none |
|---|---|---|---|---|
| A Platform, login, access | 5 | 5 | 0 | 0 |
| B Workspaces | 8 | 8 | 0 | 0 |
| C Runs | 13 | 11 | 2 | 0 |
| D Templates on a workspace | 6 | 6 | 0 | 0 |
| E State | 5 | 5 | 0 | 0 |
| F Terraform CLI | 5 | 5 | 0 | 0 |
| G Variables | 4 | 4 | 0 | 0 |
| H Projects | 5 | 5 | 0 | 0 |
| I VCS | 9 | 8 | 1 | 0 |
| J Registry | 6 | 6 | 0 | 0 |
| K Catalog and builder | 5 | 4 | 1 | 0 |
| L Rollouts | 4 | 4 | 0 | 0 |
| M Drift | 2 | 2 | 0 | 0 |
| N Audit | 2 | 2 | 0 | 0 |
| O Account, organization | 3 | 3 | 0 | 0 |
| P Operations | 6 | 5 | 1 | 0 |
| Q Runner lifecycle | 8 | 7 | 1 | 0 |
| R Environments, promotion | 5 | 4 | 1 | 0 |
| **Total** | **101** | **94** | **7** | **0** |

A1, I1 and J2 are counted by their first verdict (`full`); their `partial` halves are listed in the gaps.

---

## Gaps (for step 2)

### P1 — core deploy / state / access / security, not covered end to end

None left (closed in step 2: `http/flows_test.go`, `http/cloud_gotfe_test.go:TestCLIForceUnlock`, `e2e/e2e_test.go:TestRealRunner`).

### P2 — secondary flows

None left (closed in step 2: `http/flows_test.go` (`TestDriftChecks`, `TestRollout`, `TestDefaultProject`, `TestVariableSetLifecycle`, `TestDeleteVCSConnection`, `TestExecutionMode`), `http/server_test.go:TestLogout`, `http/audit_test.go:TestAuditLogTampered`, the runner-token column of `http/access_test.go:TestAccessMatrix`, `http/workspacetemplates_test.go:TestSensitiveInputs` (`Secrets`), `e2e/e2e_test.go:TestRealRunner` (template removal, sensitive input masked), `app/state_test.go:TestReleaseOrphanedLocks`, `app/vcsruns_test.go:TestPollVCSRuns`, `app/rotate_test.go:TestRotateKeyRace`, `cmd/opsolete/main_test.go:TestRotateKeyCLI`).

### P3 — nice to have (7)

Closed alongside P2: `opsolete token` (`cmd/opsolete/main_test.go:TestCreateToken`), the 256 KB log post limit (`http/logs_test.go:TestLogPostLimit`).

1. I1 Azure DevOps through the HTTP flows (connect, link, catalog write) — http e2e with a fake ADO.
2. K4 Delete a template in PR mode opens its own PR — http e2e.
3. P4 `/readyz` 503 without DB; `METRICS_ADDR` listener — http.
4. C12 Live SSE woken by a new chunk while the run is active — http.
5. C13 Organization currency reaching the job — http e2e.
6. A1 `serve()` startup refusals (short `RUNNER_TOKEN`, unusable `STATE_STORE`) — needs `serve` split for testability.
7. Q1 Runner refuses to start without child subreaper — runner (Linux).

---

## Docs vs implementation

Checked page by page against the code in Phase 8 step 4: the mismatches listed here before are fixed in the docs. `internal/config` `TestDocumented` (env vars in `config/reference.md`) and `internal/http` `TestEndpointsDocumented` (routes in `endpoints.md`, both ways) keep the two reference pages from drifting.

Remaining, code rather than docs (not changed):

- NixOS has no option for `METRICS_ADDR` or `SECRET_KEY_PREVIOUS` (documented: use `environment` / `secretFiles`).
