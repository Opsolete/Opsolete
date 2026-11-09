---
title: Projects
weight: 2
---

A **project** groups [workspaces]({{< relref "workspaces" >}}). Team access and [variable sets]({{< relref "variables" >}}) given to a project reach all of its workspaces.

- **Every workspace is in exactly one project.** New workspaces go into the project picked when creating them, or the **default project**. A new installation has one project, `default`, which is the default.
- **Workspace names stay unique across all projects**, so a workspace's URL, its state address and Terraform's `cloud` block don't change when it moves. Projects group workspaces; they don't add a namespace.

## The project menu

**Projects** in the menu lists the projects you can see, with their team and workspace counts; search by name. A project has its own menu:

- **Overview:** its ID and description, and the most recently active workspaces. **+ New workspace** creates one in this project, if you [may](#access).
- **Workspaces:** all of the project's workspaces you can see.
- **Environment values:** the project's values for template inputs marked per environment; they apply to each of its workspaces at every run. Admins and groups with `operate` on the project set them. See [Environments and promotion]({{< relref "environments" >}}).
- **Settings** (admins only): *General* (name, description, default project, delete), *Team access* and *Variable sets*.

Everyone who can see a project sees the first three; only admins see and change its settings.

## Manage projects

Admins:

- **Create** a project with **+ New project** on the projects page. Its name follows the workspace name rule and is its URL: renaming a project changes its URL, not its workspaces'.
- **Make another project the default** under *Settings › General*. The default project can't be deleted.
- **Delete** an empty project under *Settings › General*: its team access and variable set attachments go with it. Move or delete its workspaces first.
- **Move a workspace** to another project in the workspace's *Settings › General*. That's allowed during a run: its variables are resolved when each phase starts, so the next phase gets the new project's sets. Its [per-environment values]({{< relref "environments#per-environment-inputs" >}}) switch too, from the next run.

Every project change and move is in the [audit log]({{< relref "/docs/operations/authentication#audit-log" >}}).

## Access

An admin grants an IdP group `read` or `operate` on a project under *Settings › Team access*. The grant reaches every workspace in the project. A user's level on a workspace is the highest of: being its creator (operate), a grant on the workspace, and a grant on its project. A workspace's *Team access* page lists the grants it gets from its project.

You see a project if you can see one of its workspaces or hold a grant on it; everyone sees the default project.

**Creating a workspace in a project needs `operate` on the project**; admins create workspaces in any project. That includes the default project: only admins create workspaces there, unless a group is granted `operate` on it. A new workspace gets the project's variable sets, so `read` must not be enough to receive them. Where you may create workspaces nowhere, the pages say to ask an admin to create one, or for `operate` on a project.

## Variable sets

An admin attaches a variable set to projects on the set's page (*Settings › Variable sets*); a project's *Settings › Variable sets* lists them. They apply to every workspace in the project **except repository-linked ones**: whoever operates a workspace can link it to any code, and a project set, unlike one attached to the workspace, isn't detached when the link changes. Attach a set to a repository-linked workspace itself instead. For the order between a workspace's variables and sets, see [Precedence]({{< relref "variables" >}}).

## VCS connections

An admin can open a [VCS connection]({{< relref "/docs/catalog/vcs-providers#which-workspaces-may-link-a-connection" >}}) to selected projects: every workspace of those projects may link its repositories, including new ones created there, without selecting each workspace. A workspace moved out of the project loses the connection, and its runs fail when they start.

## Terraform CLI

Terraform's `cloud` block names a project with `workspaces { project = "…" }`: `terraform init` creates a missing workspace there. See [Terraform CLI]({{< relref "terraform-cli" >}}).
