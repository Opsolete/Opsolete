---
title: Endpoints
weight: 6
---

| Endpoint | Purpose |
|---|---|
| `/healthz` | Liveness |
| `/readyz` | Readiness (checks the DB) |
| `/` | Templates |
| `/templates/{id}` | Form, creates a workspace |
| `/workspaces` | Workspace list |
| `/workspaces/new` | Blank, VCS-linked or from a template |
| `/workspaces/{name}` | Overview, runs, outputs, resources, state versions |
| `GET\|POST /workspaces/{name}/vcs` | Version control settings |
| `POST /workspaces/{name}/state-versions/{id}/rollback` | Roll back state |
| `POST /workspaces/{name}/runs` | `action=apply\|destroy\|refresh\|assess`, `replace=<address>` for apply |
| `POST /workspaces/{name}/settings` | Name, auto-apply, drift detection |
| `POST /workspaces/{name}/delete` | Delete a workspace |
| `POST /workspaces/{name}/access` | Grants (admins) |
| `GET\|POST /workspaces/{name}/configure` | Configure the workspace's template inputs |
| `GET\|POST /workspaces/{name}/templates/new?template={id}` | Add a template |
| `/workspaces/{name}/templates/{module}` | Configure an added template |
| `POST /workspaces/{name}/templates/{module}/remove` | Remove an added template |
| `/workspaces/{name}/runs/{id}` | Plan, confirm/discard/cancel, live log, history |
| `/workspaces/{name}/runs/{id}/logs` | Raw log |
| `/auth/login`, `/auth/callback`, `/auth/logout` | Login |
| `/registry`, `/registry/publish` | Module registry |
| `/vcs`, `/vcs/new` | VCS providers, add-provider wizard |
| `GET\|POST /vcs/{id}/settings` | Connection scope (admins) |
| `/v1/modules/…` | Registry protocol |
| `/v1/vcs-archives/…` | Signed, for runners |
| `/api/runner/v1/…` | Runner API |
| `/api/state/v1/{id}` | Terraform state backend |
| `/api/v2/…` | Cloud block API, user tokens |
| `/oauth/authorization`, `/oauth/token` | `terraform login` |
