---
title: About Opsolete
weight: 1
---

Opsolete takes you from forms to infrastructure. Automatically.

Pick a template, name a **workspace** and fill in a form generated from JSON. A **Opsolete runner** plans the pinned Terraform module, you review the plan and confirm it, and a runner applies exactly that plan. You watch the output live in the browser, and the Terraform outputs show up when it's done.

## How it fits together

- **Opsolete server:** authentication · JSON forms · module registry · PostgreSQL · job queue · log storage and streaming · Terraform state backend (versions in blob storage, locks in PostgreSQL).
- **Opsolete runners:** pull jobs and run the approved module with Terraform, with the credentials the job's variables carry.
- **Terraform + existing modules:** plan · apply · outputs, with state in the Opsolete `http` backend, one state per workspace.

Runners never touch the database or the state store directly. The server holds read/write access to the state store, and cloud credentials only as sealed variables it never uses itself.

Opsolete is one Go binary (server and runner modes) that needs PostgreSQL and, for state, a blob store (Azure Blob, S3, GCS, or a local folder). Kubernetes is the target platform: the server and the runners run as separate Deployments, from the `app` and `runner` images of that binary; a NixOS module runs both as services.

## Where it's at

Opsolete is at 0.8 and heading for 1.0. Version 0.6 was a rebuild and needs a fresh install: see [Upgrading]({{< relref "/docs/install/upgrading" >}}). What's planned is on the [roadmap]({{< relref "/docs/roadmap" >}}), and what changed in each release in the [release notes]({{< relref "/docs/install/release-notes" >}}).
