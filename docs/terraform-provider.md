# Managing Opsolete with Terraform: API design (phase 1)

Status: **agreed** (November 2026); phase 1 is being built. Phase 2, the provider (`terraform-provider-opsolete`, its own
repository), is built on this API.

## Goal and scope

A Terraform provider manages Opsolete's platform objects: **projects and their environment values, workspaces (empty,
with their settings), workspace variables, variable sets (their variables and where they apply) and VCS
connections.**

Never managed with Terraform, always in the UI: templates and templates on workspaces, runs, team access (grants).
Not in this phase: the template catalog settings, registry modules.

Everything below already exists in the service layer (`internal/app`: validation, access rules, audit log); phase 1
adds the JSON API for it, identifiers Terraform can keep, and a token for automation.

## Decisions

1. **Identified by ID, not by name.** Terraform keeps an object's ID in its state; with names, a rename in the UI
   would look like a deleted and a new object. Every object is addressed by its UUID (`/projects/{id}`), and lists
   filter by name (`GET /projects?name=prd`), which is how the provider looks up objects for `terraform import` by
   name and in data sources.
   - **Breaking, on purpose** (no backward compatibility before 1.0): the existing `/api/v1/workspaces/{name}/…`
     routes (templates on a workspace, runs) move to `/api/v1/workspaces/{id}/…`. One way to address an object is
     simpler than two (a workspace name may itself look like a UUID, so a path segment can't safely mean either).
2. **A service token for automation.** User tokens expire and act as a person. An admin creates a **service token**:
   a name, admin rights, no expiry unless given, revocable, shown once, and every action it takes is in the audit log
   as `token:<name>`. Created under *Settings → API tokens*, or for bootstrapping (Terraform needs a token before it
   can create anything) with the CLI, like runner tokens: `opsolete api-token <name>` or
   `opsolete api-token -from-env NAME <name>`. Sent as `Authorization: Bearer …`, like user tokens.
3. **Write-only secrets.** Sensitive variable values and VCS tokens are never returned (`"value": null,
   "sensitive": true`). The provider keeps the configured value in its state and can't see a change made in the UI;
   the docs say so (as Terraform's `tfe` provider does).
4. **Attachments are their own objects.** A variable set applies to projects and workspaces through
   `PUT`/`DELETE /variable-sets/{id}/projects/{project_id}` (and `/workspaces/{workspace_id}`), one at a time, so a
   workspace's configuration can attach a set without owning the set's whole list. A set also stays global or not
   (`global`).
5. **Same rules as the UI.** Every endpoint calls the same app function as its form: the same access checks
   (projects, variable sets and VCS connections are admin-only; a workspace and its variables need operate), the same
   validation (`422` with `fields`), the same audit log. Errors keep today's format: `{"error": "…", "fields": {…}}`
   with `401`, `403`, `404`, `409` and `422`.
6. **Updates are `PATCH` of the fields sent.** A field not sent stays as it is, so the provider sends exactly what
   the configuration manages. A field that can't change in place (a variable's key, a VCS connection's provider) is
   refused with `422`; the provider replaces the object instead.

## Endpoints (`/api/v1`)

| Object | Endpoints | Fields |
|---|---|---|
| Project | `GET/POST /projects`, `GET/PATCH/DELETE /projects/{id}` | `name`, `description`, `runner_pool`, `default` (read: `id`) |
| Environment value | `GET /projects/{id}/environment`, `PUT/DELETE /projects/{id}/environment/{template_id}/{input}` | `value`, `keep_own` (read: `template_id`, `input`, `set_by`, `set_at`); checked by the input's rules. Admins and operators of the project |
| Workspace | `GET/POST /workspaces`, `GET/PATCH/DELETE /workspaces/{id}` | `name`, `project_id`, `execution_mode` (`remote`/`local`), `auto_apply`, `drift_detection` (read: `id`, `created_at`). `POST` with `template` and `inputs` still creates one from a template, as today. `DELETE` refuses while its state manages resources, unless `?force=1` (admins) |
| Workspace variable | `GET/POST /workspaces/{id}/variables`, `GET/PATCH/DELETE /workspaces/{id}/variables/{variable_id}` | `category` (`env`/`terraform`), `key`, `value`, `sensitive`, `hcl` |
| Variable set | `GET/POST /variable-sets`, `GET/PATCH/DELETE /variable-sets/{id}` | `name` (fixed), `description`, `global`, `priority` (read: `project_ids`, `workspace_ids`) |
| Set variable | `…/variable-sets/{id}/variables[/{variable_id}]` | as a workspace variable |
| Set attachment | `PUT/DELETE /variable-sets/{id}/projects/{project_id}`, `PUT/DELETE /variable-sets/{id}/workspaces/{workspace_id}` | none (idempotent) |
| VCS connection | `GET/POST /vcs-connections`, `GET/PATCH/DELETE /vcs-connections/{id}` | `name` (fixed), `provider` (`github`/`azure-devops`, fixed), `base_url` (fixed), `token` (write-only; a new one is checked like in the UI), `scope` (`""`/`all`/`selected`), `project_ids`, `workspace_ids` |
| Service token | *Settings → API tokens*, `opsolete api-token` | `name`, `expires_at` (optional) |

Lists aren't paginated: these objects number in the hundreds at most. If that changes, `?page` comes later without
breaking anything.

## The provider (phase 2), for orientation

```hcl
provider "opsolete" {
  url   = "https://opsolete.example.com"
  token = var.opsolete_token # a service token; or OPSOLETE_TOKEN
}

resource "opsolete_project" "prd" {
  name        = "prd"
  runner_pool = "prd"
}

resource "opsolete_variable_set" "azure_prd" {
  name = "azure-prd"
}

resource "opsolete_variable_set_variable" "secret" {
  variable_set_id = opsolete_variable_set.azure_prd.id
  category        = "env"
  key             = "ARM_CLIENT_SECRET"
  value           = var.arm_client_secret
  sensitive       = true
}

resource "opsolete_variable_set_project" "azure_prd" {
  variable_set_id = opsolete_variable_set.azure_prd.id
  project_id      = opsolete_project.prd.id
}

resource "opsolete_workspace" "shop" {
  name       = "shop-prd"
  project_id = opsolete_project.prd.id
}
```

Resources: `opsolete_project`, `opsolete_environment_value`, `opsolete_workspace`, `opsolete_workspace_variable`, `opsolete_variable_set`,
`opsolete_variable_set_variable`, `opsolete_variable_set_project`, `opsolete_variable_set_workspace`,
`opsolete_vcs_connection`; data sources for projects, workspaces and variable sets by name. All importable, by ID or
name.

## Things to know when using it

- **Two sources of truth.** A change made in the UI to a managed object is reverted by the next apply, as with any
  provider.
- **Secrets in Terraform's state.** Sensitive values and VCS tokens Terraform manages end up in its state; keep that
  state encrypted (in Opsolete itself, for instance).

## Phase 1 work

1. Storage: look up projects and variable sets by ID (workspaces and VCS connections can already); attach and detach a
   single set (the queries exist); service tokens (a token scope, no user).
2. The service token: the principal `token:<name>` with admin rights, *Settings → API tokens*, `opsolete api-token`.
3. The endpoints above (`internal/http/api_*.go`), each a thin handler on the existing app function; the existing
   name routes move to IDs.
4. Tests: a create–read–update–delete round trip per object over HTTP, and every route in the access test (each role,
   each route).
5. Docs: `endpoints.md`, the service token in `config/authentication.md`.
