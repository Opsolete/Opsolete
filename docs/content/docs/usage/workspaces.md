---
title: Workspaces
weight: 1
---

A workspace holds exactly one Terraform [state]({{< relref "state" >}}). Every change to it is a [run]({{< relref "runs" >}}).

## Create a workspace

- **From a template:** pick a template, name the workspace and fill in the form.
- **Workspaces don't need a template** (**Workspaces → New workspace**). A blank workspace can stay empty or hold only [variables]({{< relref "variables" >}}).

Runs need configuration: a template, a linked branch, or added templates.

## Link a VCS repository branch

A workspace can be linked to a **VCS repository branch** (GitHub or Azure DevOps, through a [VCS connection]({{< relref "/docs/catalog/vcs-providers" >}}), if the connection allows the workspace). Runs then plan the repository root at the branch's latest commit. That commit is pinned for the run, so the apply uses the configuration that was planned.

The server checks the branch every minute (no webhooks, so Opsolete can stay internal; see [API usage of VCS polling]({{< relref "/docs/catalog/vcs-providers#api-usage-of-vcs-polling" >}})):

- **Start a run on every new commit** (on by default for new links) queues an apply run of each new commit that has no run yet, applied with auto-apply or after confirmation.
- **Speculative plans for pull requests** (opt-in) queue a plan-only run, never applied, for each new commit of an open pull request into the branch; pull requests from forks are skipped.

A workspace still has one active run at a time, so VCS runs wait their turn; if several commits land meanwhile, only the newest gets a run. Both settings are on the workspace's **Version control** page, which also shows whether the link works (**Connected to VCS**: the connection allows the workspace and the branch or tags can be read).

**Tag-based** triggers replace both: a new tag in a chosen format (semantic version `1.2.3`, with a prefix `v1.2.3`, with a suffix `1.2.3-rc`, or a custom regular expression) queues an apply run of its commit, manual runs plan the newest matching tag (by the version number in its name; tags without one are never picked), and pull requests get no plans.

## Add templates to a workspace

Add any number of templates (**Deploy…** in a workspace's menu, operate access). Each becomes a module block, `module.<name>`, next to the workspace's own configuration (the template it was created from, its branch, or nothing), in the same state; add one template several times under different names.

- Adding, configuring or removing one starts a plan; removing one plans the destruction of its resources.
- Removing the last one (the workspace's only configuration) starts a destroy run instead, and the template goes once that destroy is applied.
- The template the workspace was created from (the *root* row) can be removed the same way. Alone, that's a destroy of everything. Next to added templates, the destroy targets only the root's own resources, so the added templates and their resources stay. That needs an indexed state (run a plan first), and the added templates must configure their own providers. The root goes once the destroy is applied; with none of its resources in the state, it goes right away.
- Their outputs show as `<name>__<output>`.
- Workspace variables and variable sets reach every template: env variables (credentials) as the environment, Terraform variables as the root's variables and as inputs of each added template whose module declares them (a template's own inputs win). So a workspace needs no root template. Added templates on external sources (e.g. `git::`) get no Terraform variables: publish their module to the registry.
- Each template's module should configure its own providers (see [Writing modules for templates]({{< relref "/docs/catalog/templates#writing-modules-for-templates" >}})). Otherwise it depends on provider blocks in the workspace's own configuration.

## Update to a new module version

When a template is moved to a newer module version, workspaces on an older one show *A new module version is available*. **View update** (or **Configure…** at any time) shows the form filled with the workspace's inputs, plus any new ones. Saving moves the workspace to the template's version and queues a plan, which is confirmed as usual.

## Rename and delete

- **Rename** a workspace from its page. State, runs and grants are keyed by its id and stay put.
- **Delete** a workspace from its page once its state manages no resources (destroy them first): runs, variables, grants and state files go with it.

## Access

The creator and admins operate a workspace; IdP groups get `read` or `operate` per workspace. Without access, a workspace returns 404, as if it didn't exist. See [Teams: access per workspace]({{< relref "/docs/operations/authentication#teams-access-per-workspace" >}}).
