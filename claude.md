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
- **Deployment execution:** Form2Stack runners (`form2stack runner`, own container image).
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

Decision (October 2026): deployments run on **Form2Stack's own runners**. The earlier Azure Pipelines executor has been removed, leaving a single execution path.

- The server owns all state. Runs in PostgreSQL double as the job queue (`FOR UPDATE SKIP LOCKED`).
- Runners pull jobs over HTTPS with a runner-scoped token, so they need no database access and no inbound connectivity. Each claim returns a lease id (fencing token) that every later write must present. Leases expire without heartbeats, and the server then fails the job instead of retrying it.
- Runners execute Terraform through `terraform-exec` (init → plan → apply → output), one job at a time, in a throwaway working directory, as a non-root user.
- Runners stream output to the server, which stores it and serves it to the browser via Server-Sent Events. Sensitive outputs never leave the runner.
- A **workspace** holds one Terraform state; every change to it is a **run** (HCP-style): a plan phase, a human confirmation (or per-workspace auto-apply; destroys always ask), then an apply phase that applies the saved plan. Each phase is a separate job; the plan file and lock file travel between them via the state store. One active run per workspace. Runs can be canceled gracefully (runner interrupts Terraform, which still saves state) or forced.
- Decision (October 2026): workspaces are not bound to templates. A workspace's configuration comes from a template, a VCS branch (the server serves the run's pinned commit as a signed archive), or nowhere: it may hold only variables, or nothing. Decision (October 2026): any number of templates can be added to a workspace; each is a module block (`module.<name>`) the runner writes next to the root configuration, in the same state. (Manual state upload was removed in October 2026: state only comes from runs; old versions can be rolled back to.)
- Decision (October 2026, replaces runner pools): cloud credentials are **variables**, HCP-style. Env and Terraform variables live on a workspace or in variable sets (global or attached to workspaces), AES-GCM sealed with `SECRET_KEY` in PostgreSQL, and are handed to the runner with each claimed job. Runners are generic and hold no cloud credentials of their own.
- Team access = IdP `groups` claim (recorded per user at login). Admins grant groups read/operate per workspace; without access a workspace is a 404.
- Decision (October 2026): Form2Stack is the state backend, HCP-style. Runners use Terraform's `http` backend against the server with a per-run credential (HMAC of workspace + run, valid only while the run is planning or applying). The server stores immutable state versions in blob storage via Go CDK (`STATE_STORE`: azblob, s3, gs, file) and locks in PostgreSQL. A resource index (names/types only) is stored per state version.
- The target platform is Kubernetes: runners scale as replicas, and the first SIGTERM drains (finishes the current job) while a second interrupts it.

## 4. Minimal Application Architecture

Implement a single Go application with clearly separated packages:

- `internal/auth` — authentication and authorization.
- `internal/templates` — JSON template loading, validation, and Git integration.
- `internal/terraform` — Terraform-related types and integrations.
- `internal/runner`, `internal/runnerapi` — the execution runner and its wire protocol.
- `internal/registry` — private Terraform module registry.
- `internal/vcs` — GitHub / Azure DevOps connections; semver tags of linked repos are published as registry versions (polled, no webhooks).
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

- **Form2Stack server:** authentication · JSON forms · module registry · PostgreSQL · job queue · log storage and streaming · Terraform state backend (versions in blob storage, locks in PostgreSQL).
- **Form2Stack runners:** pull jobs and run the approved module with Terraform via `terraform-exec`, with the credentials the job's variables carry.
- **Terraform + existing modules:** plan · apply · outputs, with state in the Form2Stack `http` backend, one state per workspace.

Runners never touch the database or the state store directly. The server holds read/write access to the state store, and cloud credentials only as sealed variables it never uses itself.
