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

- The server owns all state. Deployments in PostgreSQL double as the job queue (`FOR UPDATE SKIP LOCKED`).
- Runners pull jobs over HTTPS with a runner-scoped token, so they need no database access and no inbound connectivity. Each claim returns a lease id (fencing token) that every later write must present. Leases expire without heartbeats, and the server then fails the job instead of retrying it.
- Runners execute Terraform through `terraform-exec` (init → plan → apply → output), one job at a time, in a throwaway working directory, as a non-root user.
- Runners stream output to the server, which stores it and serves it to the browser via Server-Sent Events. Sensitive outputs never leave the runner.
- Runners own cloud and backend credentials, not the server. Every deployment gets its own state key in a remote backend with locking.
- The target platform is Kubernetes: runners scale as replicas, and the first SIGTERM drains (finishes the current job) while a second interrupts it.

## 4. Minimal Application Architecture

Implement a single Go application with clearly separated packages:

- `internal/auth` — authentication and authorization.
- `internal/templates` — JSON template loading, validation, and Git integration.
- `internal/terraform` — Terraform-related types and integrations.
- `internal/runner`, `internal/runnerapi` — the execution runner and its wire protocol.
- `internal/registry` — private Terraform module registry.
- `internal/storage` — PostgreSQL access and repositories.
- `internal/http` — HTTP handlers, routing, and middleware.
- `internal/config` — application configuration.

Use this structure as a starting point, not as a reason to introduce unnecessary abstractions. Keep related functionality together and avoid creating packages that contain only pass-through wrappers.

## 5. Implementation Constraints

- Use PostgreSQL from the beginning.
- Use official HashiCorp Terraform Go libraries where appropriate.
- Reuse existing Terraform modules instead of rewriting infrastructure definitions.
- Avoid shell-command construction and arbitrary command execution from user input.
- Do not store Terraform state files in PostgreSQL; persist state references and deployment metadata instead.
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

- **Form2Stack server:** authentication · JSON forms · module registry · PostgreSQL · job queue · log storage and streaming.
- **Form2Stack runners:** pull jobs, run the approved module with Terraform via `terraform-exec`, and hold the cloud and backend credentials.
- **Terraform + existing modules:** plan · apply · outputs · state, in a remote backend with locking and one state key per deployment.

Runners never touch the database, and the server never holds cloud credentials.
