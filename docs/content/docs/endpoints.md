---
title: Endpoint reference
weight: 6
aliases:
  - /docs/operations/endpoints/
---

Every route the server registers. A test (`TestEndpointsDocumented`) fails when a route is missing here, and `TestAccessMatrix` checks who may call each one.

**Access:** *public* needs nothing; *user* is any signed-in user; *read* / *operate* is that level on the workspace or project (as its creator, through a group grant on it or on its project, or as an admin); *admin* is the admin role. In the browser, a page without a session redirects to the login (`302`) and a form post without one is `401`. A workspace or project you can't see is `404`, as if it didn't exist; too little access to one you can see, or a non-admin on an admin route, is `403`.

**Components:** with `DEPLOYMENTS_ENABLED=false` only health, login, account, organization, audit, the registry and VCS routes exist (`/` redirects to `/registry`); with `REGISTRY_ENABLED=false` the `/registry` and `/v1/modules`, `/v1/archives` routes don't. The VCS and catalog routes exist when deployments or the registry are on.

## Public

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /healthz` | public | Liveness |
| `GET /readyz` | public | Readiness (checks the database) |
| `GET /static/{file}` | public | CSS and JavaScript, `<name>.<hash>.<ext>`, cached immutable |
| `GET /.well-known/terraform.json` | public | Terraform service discovery (`modules.v1`, `tfe.v2`, `login.v1`) |
| `GET /auth/login` | public | Starts the OIDC login |
| `GET /auth/callback` | public | OIDC redirect URI (`PUBLIC_URL/auth/callback`) |
| `POST /auth/logout` | public | Ends the session |

## Templates and the catalog

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /{$}` | user | The template catalog (`/`) |
| `GET /templates/{id}` | user | A template's form |
| `POST /templates/{id}` | operate on the chosen project | Creates a workspace from the template (`ops-workspace`, `ops-project`, the inputs) and queues its plan |
| `GET /templates/new` | admin, builder on | Template builder |
| `POST /templates/new` | admin, builder on | Saves a new template, or `preview=1` shows its form |
| `GET /templates/{id}/edit` | admin, builder on | Builder for an existing template |
| `POST /templates/{id}/edit` | admin, builder on | Saves it (or `preview=1`) |
| `POST /templates/{id}/delete` | admin, builder on | Removes the template (a pull request or commit with a Git catalog) |
| `GET /templates/{id}/rollout` | admin | Workspaces using the template and its open rollout; `?project=` filters |
| `POST /templates/{id}/rollout` | admin | Starts a rollout: `project` (empty: all), `batch` (`0`: all at once), and the template's current `source` and `ref` (`409` if the template moved meanwhile) |
| `POST /templates/{id}/rollout/{action}` | admin | `pause`, `resume` or `stop` the open rollout |
| `GET /catalog` | admin | Settings → Templates: the Git catalog |
| `POST /catalog` | admin | Sets its connection, repository, branch, folder and save mode |
| `POST /catalog/sync` | admin | Sync now |
| `POST /catalog/delete` | admin | Disconnect: back to `TEMPLATES_DIR` |

## Workspaces

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /workspaces` | user | Workspace list |
| `GET /workspaces/new` | user | New workspace: blank, VCS-linked or from a template |
| `POST /workspaces/new` | operate on the chosen project | Creates a blank or VCS-linked workspace |
| `GET /workspaces/{name}` | read | Overview: latest run, outputs, resources, settings summary |
| `GET /workspaces/{name}/runs` | read | Run list |
| `GET /workspaces/{name}/states` | read | State versions |
| `GET /workspaces/{name}/states/{version}` | operate | A state version and its diff (values) |
| `POST /workspaces/{name}/state-versions/{version}/rollback` | operate | Roll back state to this version |
| `GET /workspaces/{name}/variables` | read | Variables and the variable sets that apply |
| `POST /workspaces/{name}/variables` | operate | Sets a variable: `category` (`env` or `terraform`), `key`, `value`, `sensitive` |
| `POST /workspaces/{name}/variables/{id}/delete` | operate | Deletes a variable |
| `GET /workspaces/{name}/deploy` | read | Templates to add |
| `GET /workspaces/{name}/templates/new` | operate | Form to add `?template={id}`; with `&found={run}&from={module}`, filled in with what that import plan found for the module (`404` if nothing) |
| `POST /workspaces/{name}/templates/new` | operate | Adds it and queues a plan |
| `GET /workspaces/{name}/templates/{module}` | operate | Configure an added template (or update it to the template's version); with `?found={run}`, filled in with what that import plan found for it (`404` if nothing) |
| `POST /workspaces/{name}/templates/{module}` | operate | Saves it and queues a plan |
| `POST /workspaces/{name}/templates/{module}/remove` | operate | Removes it: a plan destroying its resources, or at once if nothing is deployed |
| `GET /workspaces/{name}/promote` | read | Pick a promotion target; with `?target={workspace}` (operate on it) or `?new={name}&project={project}` (operate on the project) the preview of what changes there |
| `POST /workspaces/{name}/promote` | read; operate on the target | Promotes: `target` or `new` + `project`, `digest` (the preview's), the asked-for inputs as `{module}.{input}` (sensitive: `secret.{module}.{input}`); queues a plan on the target that waits for confirmation (a destroy plan first if it removes or replaces a deployed template; its apply run follows once that is applied) |
| `GET /workspaces/{name}/vcs` | operate | Version control settings |
| `GET /workspaces/{name}/vcs/edit` | operate | Change the linked repository or branch |
| `POST /workspaces/{name}/vcs` | operate | Links, changes or unlinks a repository branch and its triggers |
| `GET /workspaces/{name}/settings` | operate | Settings › General |
| `GET /workspaces/{name}/settings/{page}` | operate | Settings pages: `health`, `lock`, `access`, `deletion` |
| `POST /workspaces/{name}/settings` | operate | General: `name`, `execution_mode` (`remote` or `local`), `auto_apply` |
| `POST /workspaces/{name}/settings/remote-state` | operate | Remote state sharing: `remote_state=all\|specific`, `remote_state_consumers` |
| `POST /workspaces/{name}/settings/health` | operate | Health: `drift_detection` |
| `POST /workspaces/{name}/lock` | operate | `action=lock` (optional `reason`, at most 300 characters), `unlock` or `force-unlock` |
| `POST /workspaces/{name}/access` | admin | Grants a group `read` or `operate` (`group`, `level`; empty level revokes) |
| `POST /workspaces/{name}/project` | admin | Moves the workspace to `project` |
| `POST /workspaces/{name}/delete` | operate | Deletes the workspace; refused while its state manages resources unless `force=1` |

## Runs

| Endpoint | Access | Purpose |
|---|---|---|
| `POST /workspaces/{name}/runs` | operate | Starts a run: `action=apply\|plan\|destroy\|refresh\|assess`, `replace=<address>` (apply only, repeatable); `409` while another run is active |
| `GET /workspaces/{name}/runs/{run}` | read | Plan, confirm / discard / cancel, live log, history; an import plan's found values (operate) |
| `POST /workspaces/{name}/runs/{run}/confirm` | operate | Queues the apply of a plan waiting for confirmation |
| `POST /workspaces/{name}/runs/{run}/discard` | operate | Ends it without applying |
| `POST /workspaces/{name}/runs/{run}/change` | operate | Change settings: discards an import plan waiting for confirmation, and redirects to the run's template's form (configure, or add again when the run's end removed it) with what was entered for it and what its plan found; for an import plan, or a run that changed a template and ended without applying; `404` otherwise, `409` while planning |
| `POST /workspaces/{name}/runs/{run}/plan-again` | operate | An import's plan round: plans a new version with what it found (`409` if it found nothing new) |
| `POST /workspaces/{name}/runs/{run}/adopt` | operate | An import's plan round: queues the apply run of exactly its version (`409` if it can't: the reason says why) |
| `POST /workspaces/{name}/runs/{run}/cancel` | operate | Cancels; `force=1` cuts off a runner that doesn't react |
| `GET /workspaces/{name}/runs/{run}/logs` | read | Raw log |
| `GET /workspaces/{name}/runs/{run}/logs/stream` | read | Live log (Server-Sent Events); at most 8 streams per user per replica, else `429` |

## Projects

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /projects` | user | Projects you can see |
| `POST /projects` | admin | Creates a project (`name`, `description`) |
| `GET /projects/{project}` | read, or any user for the default project | Overview |
| `GET /projects/{project}/workspaces` | same | Its workspaces |
| `GET /projects/{project}/environment` | same | Environment values: the project's values for per-environment template inputs |
| `POST /projects/{project}/environment` | admin, or operate on the project | Sets one (`template`, `input`, `value`, `keep_own` checked: workspaces with their own value keep it) |
| `POST /projects/{project}/environment/clear` | admin, or operate on the project | Clears one (`template`, `input`) |
| `GET /projects/{project}/settings` | admin | Settings › General |
| `GET /projects/{project}/settings/{page}` | admin | `access` (team access), `variable-sets` |
| `POST /projects/{project}/settings` | admin | `name`, `description`, `default`, `runner_pool` |
| `POST /projects/{project}/access` | admin | Grants a group `read` or `operate` |
| `POST /projects/{project}/delete` | admin | Deletes an empty, non-default project |

## Variable sets

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /variable-sets` | admin | List |
| `POST /variable-sets` | admin | Creates one (`name`, `description`, `global`; optionally attached to `workspace` or `project`) |
| `GET /variable-sets/{set}` | admin | A set: variables, workspaces, projects |
| `POST /variable-sets/{set}` | admin | `description`, `global` |
| `POST /variable-sets/{set}/variables` | admin | Sets a variable |
| `POST /variable-sets/{set}/variables/{id}/delete` | admin | Deletes a variable |
| `POST /variable-sets/{set}/workspaces` | admin | The workspaces it's attached to (`workspace`, repeated) |
| `POST /variable-sets/{set}/projects` | admin | The projects it's attached to (`project`, repeated) |
| `POST /variable-sets/{set}/delete` | admin | Deletes the set |

## Registry

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /registry` | user | Modules |
| `GET /registry/{ns}/{name}/{system}/{version}` | user | Generated docs of a version |
| `GET /registry/{ns}/{name}/{system}/{version}/docs.md` | user | The same as Markdown |
| `POST /registry` | admin, uploads on | Uploads a version (multipart: `namespace`, `name`, `system`, `version`, `archive`) |
| `GET /registry/publish` | admin | Publish wizard |
| `POST /registry/publish` | admin | Publishes a module from a VCS repository (`connection`, repository, folder) |
| `POST /registry/{ns}/{name}/{system}/sync` | admin | Sync now |
| `POST /registry/{ns}/{name}/{system}/unlink` | admin | Unlinks it from its repository |
| `POST /registry/{ns}/{name}/{system}/delete` | admin | Deletes every version (refused while a template uses it) |
| `GET /v1/modules/{ns}/{name}/{system}/versions` | registry or `terraform login` token | Registry protocol: versions |
| `GET /v1/modules/{ns}/{name}/{system}/{version}/download` | registry or `terraform login` token | Registry protocol: `X-Terraform-Get` with a signed archive URL (10 minutes) |
| `GET /v1/archives/{ns}/{name}/{system}/{file}` | signed URL | Module archive |
| `GET /v1/vcs-archives/{file}` | signed URL | A run's repository archive, for runners (15 minutes) |

## VCS providers

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /vcs` | admin | Connections and what uses them |
| `GET /vcs/new` | admin | Add-provider wizard |
| `POST /vcs` | admin | Adds a connection (the token is checked by listing repositories) |
| `GET /vcs/{id}/settings` | admin | Workspace scope |
| `POST /vcs/{id}/settings` | admin | Saves it: `workspaces` on, `workspace_scope=all` or the selected `workspace` and `project` values |
| `POST /vcs/{id}/token` | admin | Replaces the token |
| `POST /vcs/{id}/delete` | admin | Deletes a connection: refused while the template catalog uses it or a linked workspace has an active run; linked workspaces and modules are unlinked |

## Account, organization, audit

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /account` | user | Redirects to `/account/profile` |
| `GET /account/{page}` | user | `profile`, `sessions`, `tokens` |
| `POST /account/sessions/revoke` | user | Log out everywhere |
| `POST /account/tokens/{id}/revoke` | user | Revokes one of your `terraform login` tokens |
| `POST /account/tokens/revoke` | user | Revokes all of them |
| `POST /users/{id}/sessions/revoke` | admin | Ends a user's sessions and revokes their `terraform login` tokens (from the audit log) |
| `GET /organization` | admin | Settings → Organization |
| `POST /organization` | admin | `currency` (Infracost), `module_uploads` |
| `GET /organization/runners` | admin | Settings → Runners (with deployments) |
| `POST /organization/runners` | admin | Creates a runner (`name`, `pool`) and shows its token once |
| `POST /organization/runners/{id}/delete` | admin | Deletes a runner: its token stops working |
| `GET /audit` | admin | Audit log; `?show=access` for sign-ins and refused requests |

## JSON API

`/api/v1`: the UI's operations for scripts and other front ends, with the same access as the UI (the access column). A signed-in browser uses its session (a cross-site `POST` is refused); a script sends a `terraform login` token as `Authorization: Bearer <token>`. Bodies are JSON; input values are typed (`"size": 2`, `"tags": {"env": "prd"}`), a sensitive one omitted keeps its stored value. Errors are `{"error": "…", "fields": {"name": "…", "inputs.<input>": "…"}}` (fields as the body names them) with the UI's statuses (`401`, `403`, `404`, `409` a conflict such as an active run, `422` invalid input).

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /api/v1/templates` | user | The catalog |
| `GET /api/v1/templates/{id}` | user | A template and its deploy form (`fields`: type, value, required, visible, options, locked by `?project=`) |
| `GET /api/v1/workspaces` | user | Workspaces you can see (`?project=`), with their latest run |
| `POST /api/v1/workspaces` | operate on the project | Creates a workspace from a template (`name`, `project`, `template`, `auto_apply`, `inputs`) and queues its plan: `201` |
| `GET /api/v1/workspaces/{name}` | read | A workspace and its templates with their inputs (sensitive ones never) |
| `POST /api/v1/workspaces/{name}/templates` | operate | Adds a template (`template`, `inputs`) and queues the plan: `201` with the module name |
| `GET /api/v1/workspaces/{name}/templates/{module}` | operate | An added template's configure form, its stored values |
| `PUT /api/v1/workspaces/{name}/templates/{module}` | operate | Saves its inputs (`inputs`) and the template's module version, queues the plan |
| `GET /api/v1/workspaces/{name}/runs` | read | The latest 25 runs |
| `POST /api/v1/workspaces/{name}/runs` | operate | Queues a run (`action`, `replace`): `201` |
| `GET /api/v1/workspaces/{name}/runs/{run}` | read | A run with its history and its plan's attribute changes (`diff`, values that are sensitive or hold a secret hidden) |
| `POST /api/v1/workspaces/{name}/runs/{run}/confirm` | operate | Confirms a plan that waits: `204` |
| `POST /api/v1/workspaces/{name}/runs/{run}/discard` | operate | Discards it: `204` |
| `POST /api/v1/workspaces/{name}/runs/{run}/cancel` | operate | Cancels a run (`force`); returns its status |

## Terraform CLI

`terraform login` and the [`cloud` block]({{< relref "/docs/features/terraform-cli" >}}). The `/api/v2` routes take a `terraform login` token; `{org}` must be `ORGANIZATION`.

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /oauth/authorization` | user | "Authorize Terraform CLI" page |
| `POST /oauth/authorization` | user | Issues the login code |
| `POST /oauth/token` | public (code + PKCE) | Exchanges the code for a token (30 days) |
| `GET /api/v2/ping` | public | API check |
| `GET /api/v2/organizations/{org}/entitlement-set` | token | Entitlements |
| `GET /api/v2/organizations/{org}/workspaces/{name}` | read | A workspace |
| `POST /api/v2/organizations/{org}/workspaces` | operate on the project | Creates a local-execution workspace (`422` without access to the project) |
| `GET /api/v2/organizations/{org}/projects` | token | Projects you can see (`filter[names]`) |
| `POST /api/v2/organizations/{org}/projects` | admin | Creates a project |
| `PATCH /api/v2/workspaces/{id}` | operate | Terraform's workspace update: returns the workspace, changes nothing |
| `POST /api/v2/workspaces/{id}/actions/{action}` | operate | `lock`, `unlock`, `force-unlock` (local-execution workspaces) |
| `GET /api/v2/workspaces/{id}/current-state-version` | operate | Current state version |
| `GET /api/v2/workspaces/{id}/current-state-version-outputs` | operate | Its outputs |
| `GET /api/v2/workspaces/{id}/state-versions/{version}/download` | operate | The state file |
| `POST /api/v2/workspaces/{id}/state-versions` | operate | Saves a state version (local execution, while holding the lock) |

## Runners and the state backend

| Endpoint | Access | Purpose |
|---|---|---|
| `GET /api/runner/v1/queue` | runner token | `{"queued": n}`: how many phases the runner's pool could claim now (not those waiting for a workspace's lock); for an autoscaler that starts a one-shot runner per phase |
| `POST /api/runner/v1/jobs/claim` | runner token | Long-polls for a job (up to 20 s); `204` if none |
| `POST /api/runner/v1/jobs/{id}/ack` | runner token + lease | Acknowledges a claim (within 30 s) |
| `POST /api/runner/v1/jobs/{id}/logs` | runner token + lease | Appends log output (at most 256 KB per request, else `413`); doubles as a heartbeat |
| `POST /api/runner/v1/jobs/{id}/heartbeat` | runner token + lease | Keeps the lease |
| `POST /api/runner/v1/jobs/{id}/artifacts/{name}` | runner token + lease | Uploads the saved plan or lock file |
| `GET /api/runner/v1/jobs/{id}/artifacts/{name}` | runner token + lease | Downloads it for the apply phase |
| `POST /api/runner/v1/jobs/{id}/finish` | runner token + lease | Reports the phase's result |
| `GET /api/state/v1/{id}` | run's state credential | Terraform `http` backend: read state (also a shared workspace's) |
| `POST /api/state/v1/{id}` | run's state credential | Write state (apply phase, holding the lock) |
| `LOCK /api/state/v1/{id}` | run's state credential | Take the state lock (`423` with the holder if taken) |
| `UNLOCK /api/state/v1/{id}` | run's state credential | Release it |

`METRICS_ADDR` serves Prometheus metrics on its own listener, outside these routes.
