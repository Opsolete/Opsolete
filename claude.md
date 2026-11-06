# Technology and Architecture Requirements

## 1. Project Context

This is a greenfield project. The repository starts empty.

Build a lightweight, self-hosted, no-code infrastructure provisioning platform using Go. The platform provides a simple web interface for deploying existing Terraform modules through JSON-defined forms.

Keep the implementation minimal and use official Go libraries and established dependencies wherever possible.

## 2. Required Technology Stack

### Backend

- **Language:** Go.
- **HTTP:** Go standard library `net/http`.
- **Routing:** Standard-library `http.ServeMux`, unless a small routing library provides a clear benefit.
- **Logging:** Go standard library `log/slog`.
- **JSON:** Go standard library `encoding/json`.
- **Testing:** Go standard library `testing` and `httptest`.

### Database

- **Database:** PostgreSQL is mandatory.
- **Database access:** Prefer `database/sql` with a maintained PostgreSQL driver, such as `github.com/jackc/pgx/v5` through its `database/sql` compatibility layer.
- **Schema management:** Use version-controlled SQL migrations.
- **Queries:** Prefer explicit SQL queries and a lightweight repository layer. Consider `sqlc` for generating type-safe Go code from SQL if it simplifies maintenance.

Use PostgreSQL to persist users, roles, template assignments, deployment records, input values, execution metadata, status history, and references to Terraform state.

Do not use SQLite or an in-memory database as the primary persistence solution.

### Frontend

- Start with server-rendered HTML using Go's `html/template`.
- Keep application logic and HTTP handlers separate from rendering.
- Design clean API boundaries so Vue or React can replace the initial frontend later without requiring a backend rewrite.

### Infrastructure and CI/CD

- **Infrastructure definition:** Existing Terraform modules.
- **Template definitions:** JSON files stored in Git.
- **Deployment execution:** Opsolete runners (`opsolete runner`, own container image).
- **CI/CD:** GitHub Actions (`.github/workflows`). Releases come from release-please: PR titles are Conventional Commits, and the next SemVer is derived from them. Images go to GHCR only on release.
- **Terraform integration:** Official HashiCorp Terraform Go modules where appropriate.
- **Terraform state:** Secure remote state storage with locking.

## 3. Terraform Go Integration

Use official HashiCorp Terraform Go modules for supported programmatic interactions with Terraform.

Before implementing the integration, evaluate the official Go modules and their intended use cases:

- `github.com/hashicorp/terraform-exec` for programmatic execution of the Terraform CLI from Go.
- `github.com/hashicorp/terraform-json` for structured Terraform plan and state-related data representations.
- `github.com/hashicorp/hcl/v2` only if parsing or inspecting HCL is necessary for a specific feature.

Use these libraries only where they add value. Avoid implementing custom Terraform parsers, state-file parsers, or plan-output parsers when supported libraries already provide the required functionality.

Do not embed or reimplement the entire Terraform execution engine in the Go application.

### Execution model

Decision (October 2026): deployments run on **Opsolete's own runners**. The earlier Azure Pipelines executor has been removed, leaving a single execution path.

- The server owns all state. Runs in PostgreSQL double as the job queue (`FOR UPDATE SKIP LOCKED`).
- Runners pull jobs over HTTPS with the shared `RUNNER_TOKEN` from the server's config, so they need no database access and no inbound connectivity. Each claim returns a lease id (fencing token) that every later write must present. Leases expire without heartbeats, and the server then fails the job instead of retrying it.
- Runners execute Terraform through `terraform-exec` (init → plan → apply → output), one job at a time, in a throwaway working directory, as a non-root user.
- Runners stream output to the server, which stores it and serves it to the browser via Server-Sent Events. Sensitive outputs never leave the runner.
- A **workspace** holds one Terraform state; every change to it is a **run**: a plan phase, a human confirmation (or per-workspace auto-apply; destroys always ask), then an apply phase that applies the saved plan. Each phase is a separate job; the plan file and lock file travel between them via the state store. One active run per workspace. Runs can be canceled gracefully (runner interrupts Terraform, which still saves state) or forced.
- Decision (October 2026): workspaces are not bound to templates. A workspace's configuration comes from a template, a VCS branch (the server serves the run's pinned commit as a signed archive), or nowhere: it may hold only variables, or nothing. Decision (October 2026): any number of templates can be added to a workspace; each is a module block (`module.<name>`) the runner writes next to the root configuration, in the same state. (Manual state upload was removed in October 2026: state only comes from runs; old versions can be rolled back to.)
- Decision (October 2026, replaces runner pools): cloud credentials are **variables**. Env and Terraform variables live on a workspace or in variable sets (global or attached to workspaces), AES-GCM sealed with `SECRET_KEY` in PostgreSQL, and are handed to the runner with each claimed job. Runners are generic and hold no cloud credentials of their own.
- Team access = IdP `groups` claim (recorded per user at login). Admins grant groups read/operate per workspace; without access a workspace is a 404.
- Decision (October 2026): Opsolete is the state backend. Runners use Terraform's `http` backend against the server with a per-run credential (HMAC of workspace + run, valid only while the run is planning or applying). The server stores immutable state versions in blob storage via Go CDK (`STATE_STORE`: azblob, s3, gs, file) and locks in PostgreSQL. A resource index (names/types only) is stored per state version.
- The target platform is Kubernetes: runners scale as replicas, and the first SIGTERM drains (finishes the current job) while a second interrupts it.

## 4. Minimal Application Architecture

Implement a single Go application with clearly separated packages:

- `internal/auth` — authentication and authorization.
- `internal/templates` — JSON template loading, validation, and Git integration.
- `internal/runner`, `internal/runnerapi` — the execution runner and its wire protocol.
- `internal/registry` — private Terraform module registry.
- `internal/vcs` — GitHub / Azure DevOps connections; semver tags of linked repos are published as registry versions, and branch commits and pull requests of linked workspaces queue runs (both polled, no webhooks).
- `internal/storage` — PostgreSQL access and repositories.
- `internal/http` — HTTP handlers, routing, and middleware.
- `internal/config` — application configuration.

Use this structure as a starting point, not as a reason to introduce unnecessary abstractions. Keep related functionality together and avoid creating packages that contain only pass-through wrappers.

## 5. Implementation Constraints

- Use PostgreSQL from the beginning.
- Use official HashiCorp Terraform Go libraries where appropriate.
- Reuse existing Terraform modules instead of rewriting infrastructure definitions.
- Avoid shell-command construction and arbitrary command execution from user input.
- Do not store Terraform state files in PostgreSQL; they live in the state store. PostgreSQL holds the version list, locks and a per-version resource index.
- Never expose sensitive Terraform state or credentials through the web interface.
- Pin dependencies to compatible versions and commit `go.mod` and `go.sum`.
- Verify library compatibility and supported APIs before choosing versions.
- Keep the MVP deployable as one Go binary (server + runner modes) with PostgreSQL as the only required external dependency.
- Kubernetes is the target platform (server and runners as separate Deployments of the same binary). Do not introduce microservices, a message broker, or a generic workflow engine; the runner executes exactly one fixed Terraform flow.

## 6. First Implementation Milestone

Before building the UI, establish the minimum end-to-end foundation:

1. Initialize the Go module and application entry point.
2. Configure PostgreSQL connectivity and migrations.
3. Implement health and readiness endpoints.
4. Add structured logging and configuration loading.
5. Create the initial data model for users, templates, and deployments.
6. Add a small test suite for database access and configuration.
7. Document how to run the application locally with PostgreSQL.

Then implement JSON templates, dynamic forms, authentication, and the execution runner incrementally.

## One technical choice to get right

The division of responsibility:

- **Opsolete server:** authentication · JSON forms · module registry · PostgreSQL · job queue · log storage and streaming · Terraform state backend (versions in blob storage, locks in PostgreSQL).
- **Opsolete runners:** pull jobs and run the approved module with Terraform via `terraform-exec`, with the credentials the job's variables carry.
- **Terraform + existing modules:** plan · apply · outputs, with state in the Opsolete `http` backend, one state per workspace.

Runners never touch the database or the state store directly. The server holds read/write access to the state store, and cloud credentials only as sealed variables it never uses itself.

## Roadmap: projects (in progress)

Goal: one level above workspaces, modeled on HCP Terraform projects (https://developer.hashicorp.com/terraform/cloud-docs/projects). A project is a folder of workspaces that carries access grants and variable sets for all of them. Tick items off here as they land; record changed decisions as "Decision (month year): …" like the rest of this file.

### Design

- **HCP as it is configured now** (the rudesome org, read through its API in November 2026): there is one project, "Default Project" (`prj-…`), with 2 workspaces, no teams, tags or variable sets. The default project is an organization setting (`organization.relationships.default-project`), not a fixed name. A project carries `workspace-count`, `team-count`, `auto-destroy-activity-duration`, and a `default-execution-mode` and default agent pool. Those two are inherited from the organization unless `setting-overwrites` is set. Each workspace has `relationships.project`.
- **Every workspace is in exactly one project.** The migration creates a project named `default` and stores it as `organization.default_project_id` (the singleton table from `032_organization.sql`). It then moves every existing workspace into it. A new workspace goes into the project the user picks, or the default project if they don't pick one. Admins can make a different project the default. The current default project cannot be deleted.
- **Workspace names stay unique across the whole instance**, as in HCP (unique per organization). So `/workspaces/{name}`, the state backend path, the `cloud` API and VCS polling do not change. Projects add grouping, not a new namespace.
- **Project names** follow the workspace name rule (`validWorkspaceName`, which also reserves `new`) and are the URL key: `/projects/{name}`. Renaming a project changes its URL; workspace URLs stay the same.
- **Schema** (one migration, `040_projects.sql`):
  - `projects (id uuid PK, name text UNIQUE NOT NULL, description text NOT NULL DEFAULT '', created_at)`
  - `workspaces.project_id uuid NOT NULL REFERENCES projects ON DELETE RESTRICT`, with an index. RESTRICT means only empty projects can be deleted, as in HCP.
  - `organization.default_project_id uuid NOT NULL REFERENCES projects`, which also blocks deleting the default project.
  - `project_access (project_id, group_name, level read|operate, PK(project_id, group_name))`, the same shape as `workspace_access`.
  - `variable_set_projects (set_id, project_id, PK(set_id, project_id))`, the same shape as `variable_set_workspaces`.
- **Access:** a user's level on a workspace is the highest of: creator (operate), a workspace grant, or a grant on the workspace's project. Change only the `visible` SQL condition and `Store.Access` in `internal/storage/workspaces.go`; every caller then inherits project grants. A user sees a project if they can see at least one workspace in it or hold a grant on it. Admins create, rename and delete projects, move workspaces, and grant access. There are no per-project admins yet. Decision (November 2026): everyone sees the default project and may create workspaces in it. In any other project, only admins and groups with **operate** on the project may create workspaces (`Store.CreatableProjects`): a workspace's creator operates it and the workspace gets the project's variable sets, so a read grant must not be enough.
- **Variable precedence** (strongest first, as in HCP): workspace variables, then sets attached to the workspace, then sets attached to its project, then global sets. The existing rule that global sets skip VCS-linked workspaces stays. Decision (November 2026): project-attached sets skip VCS-linked workspaces too. Anyone with operate on a workspace can link it to any repository (`loadLinkable`), and a project set, unlike a workspace set, isn't detached when the link changes; so applying them would hand the project's secrets to whoever operates any of its workspaces. An admin who wants a set on a VCS-linked workspace attaches it to that workspace, where the detach-on-relink rule protects it. Test this in step 4.
- **Moving a workspace** to another project is admin-only. It is allowed while a run is active, because nothing at runtime depends on the project except the variables, and those are resolved when a job is claimed. Record each move in the audit log.
- **Cloud API:** support `cloud { workspaces { project = "x" } }`. Serve `GET /organizations/{org}/projects` with a name filter, and accept `relationships.project` when a workspace is created. If the CLI omits the project, use `default`. Return `relationships.project` in `writeWorkspace`. Check what the CLI sends against go-tfe before building this (`cloud_gotfe_test.go`).
- **Code layout:** `internal/storage/projects.go`, `internal/http/projects.go`, and `views/projects.html` and `views/project.html`. No new package.

### Steps

1. [x] **Data model:** write the migration, then `Store` CRUD (`Projects`, `Project`, `CreateProject`, `UpdateProject`, `DeleteProject`, which refuses non-empty projects, and `MoveWorkspace`), and add `Workspace.ProjectID/ProjectName`. Storage tests cover: existing workspaces land in `default`, deleting a non-empty project fails, and the default project cannot be deleted.
2. [x] **UI, basic:** `/projects` lists projects with workspace and group-grant counts (HCP's `workspace-count` and `team-count`). `/projects/{name}` shows a project's workspaces, settings (rename, description, delete) and a "new workspace" button. The workspace create form gets a project picker. Workspace settings get a "move to project" control. The `/workspaces` list shows and filters by project. Project actions go to the audit log.
3. [x] **Access:** add project grants (`POST /projects/{name}/access`, admin only) and change `visible` and `Access`. Tests check that a project grant reveals all of the project's workspaces, that the highest of the workspace and project levels wins, and that a user with no access gets a 404.
4. [x] **Variable sets:** let sets attach to projects (on the variable set page), and apply the precedence above in the variables query. Test the precedence order.
5. [x] **Cloud API:** add the projects endpoint, workspace creation in a project, and the project relationship on workspaces, with go-tfe tests.
6. [ ] **Later, only on request:** project tags inherited by workspaces, project-level defaults (execution mode and auto-apply, which workspaces inherit unless they override them, following HCP's `default-execution-mode` and `setting-overwrites`), auto-destroy of inactive workspaces, scoping VCS connections or remote state sharing to projects, and project-admin roles beyond read/operate.
