---
title: Introduction
weight: 1
---

Opsolete takes you from forms to infrastructure. Automatically.

Pick a template, name a **workspace** and fill in a form generated from JSON. A **Opsolete runner** plans the pinned Terraform module, you review the plan and confirm it, and a runner applies exactly that plan. You watch the output live in the browser, and the Terraform outputs show up when it's done.

## How it fits together

- **Opsolete server:** authentication · JSON forms · module registry · PostgreSQL · job queue · log storage and streaming · Terraform state backend (versions in blob storage, locks in PostgreSQL).
- **Opsolete runners:** pull jobs and run the approved module with Terraform via `terraform-exec`, with the credentials the job's variables carry.
- **Terraform + existing modules:** plan · apply · outputs, with state in the Opsolete `http` backend, one state per workspace.

Runners never touch the database or the state store directly. The server holds read/write access to the state store, and cloud credentials only as sealed variables it never uses itself.

Opsolete is one Go binary (server and runner modes) with PostgreSQL as its only required external dependency. Kubernetes is the target platform: the server and the runners run as separate Deployments of the same image.

## Where to go next

{{< cards >}}
  {{< card link="getting-started" title="Getting started" subtitle="Run Opsolete locally, with or without Nix." >}}
  {{< card link="usage" title="Using Opsolete" subtitle="Workspaces, runs, variables and state." >}}
  {{< card link="catalog" title="Templates and registry" subtitle="Templates, the template builder, the module registry and VCS providers." >}}
  {{< card link="operations" title="Deployment and operations" subtitle="Configuration, login, runners and deployment." >}}
{{< /cards >}}
