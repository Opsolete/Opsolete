---
title: Workspaces
weight: 1
aliases:
  - /docs/usage/workspaces/
---

A workspace holds exactly one Terraform [state]({{< relref "/docs/features/state" >}}). Every change to it is a [run]({{< relref "/docs/features/runs" >}}).

## Create a workspace

Every workspace is in a [project]({{< relref "/docs/features/projects" >}}): pick it when creating the workspace. Creating one needs `operate` on the project, the default project too; admins create workspaces in any project.

- **From a template:** pick a template, name the workspace and fill in the form. The template is added to the new workspace like any other (`module.<name>`, see below).
- **Workspaces don't need a template** (**Workspaces → New workspace**). A blank workspace can stay empty or hold only [variables]({{< relref "/docs/features/variables" >}}).

Runs need configuration: added templates, a linked branch, or both.

## Link a VCS repository branch

A workspace can be linked to a **VCS repository branch** (GitHub or Azure DevOps, through a [VCS connection]({{< relref "/docs/features/vcs-providers" >}}), if the connection allows the workspace). Runs then plan the repository root at the branch's latest commit. That commit is pinned for the run, so the apply uses the configuration that was planned.

The server checks the branch every minute (no webhooks, so Opsolete can stay internal; see [API usage of VCS polling]({{< relref "/docs/features/vcs-providers#api-usage-of-vcs-polling" >}})):

- **Start a run on every new commit** (on by default for new links) queues an apply run of each new commit that has no run yet, applied with auto-apply or after confirmation.
- **Speculative plans for pull requests** (opt-in) queue a plan-only run, never applied, for each new commit of an open pull request into the branch; pull requests from forks are skipped.

A push waits as pending while another run is active; if several commits land meanwhile, only the newest gets a run. Pull request plans run beside it (one per pull request). Both settings are on the workspace's **Version control** page, which also shows whether the link works (**Connected to VCS**: the connection allows the workspace and the branch or tags can be read).

**Tag-based** triggers replace both: a new tag in a chosen format (semantic version `1.2.3`, with a prefix `v1.2.3`, with a suffix `1.2.3-rc`, or a custom regular expression) queues an apply run of its commit, manual runs plan the newest matching tag (by the version number in its name; tags without one are never picked), and pull requests get no plans.

## Add templates to a workspace

Add any number of templates (**Deploy…** in a workspace's menu, operate access). Each becomes a module block, `module.<name>`, next to the workspace's own configuration (its branch, or nothing), in the same state; add one template several times under different names.

The **Resources** tab groups the state's resources by module: a group per added template (also one with nothing in the state yet, such as after a failed apply), with **Configure…** and **Remove…** when you open it, and a `root` group for the workspace's own configuration.

- Adding, configuring or removing one starts a plan; removing one plans the destruction of its resources. While the workspace's state manages no resources, it's removed at once, without a run.
- Removing the last one (the workspace's only configuration) starts a destroy run instead, and the template goes once that destroy is applied.
- Their outputs show as `<name>__<output>`. Terraform keeps all outputs after a destroy limited to one template, so once that destroy is applied, the same run plans the configuration without the template (`-refresh=false`) and applies that plan if it changes only outputs. Otherwise the log says so, and the next apply removes them.
- Workspace variables and variable sets reach every template: env variables (credentials) as the environment, Terraform variables as the variables of a linked branch's configuration and as inputs of each template whose module declares them and whose template doesn't list them (see [Settings outside the template's form]({{< relref "/docs/features/variables#settings-outside-the-templates-form" >}})). Inputs the template lists keep their value, and a hidden or empty one the module's default. Templates on external sources (e.g. `git::`) get no Terraform variables, only environment variables: publish their module to the registry (it can sync from VCS).
  - *Possible improvement:* the runner has every template's module on disk after `terraform init`, `git::` ones too. It could add the workspace Terraform variables each module declares itself, if the job also lists the inputs each template's form asks for, so those keep winning.
- Each template's module configures its own providers (see [Writing modules for templates]({{< relref "/docs/features/templates#writing-modules-for-templates" >}})).

## Import existing resources

Resources built outside Opsolete (in the Azure portal, by scripts) can be taken over without being created again,
with any template whose module can import (an `existing` input; see
[Writing modules that can import]({{< relref "/docs/features/templates#writing-modules-that-can-import" >}})):

1. **Import existing** (operate access): at the top of the template's add form (*Deploy…*), or of its deploy form
   for a new workspace, offered for templates whose module can import. Then only the key input (if the template has
   one) and required sensitive inputs are required: fill in what identifies the resource (its name, its resource group); the module finds the rest.
2. **A plan round.** Saving plans the template with its module importing (its `existing` input on), as a plan that
   is never applied: it takes no state lock, and the workspace doesn't change. The run page's **Import** section
   shows `N to import · add · change · destroy`, the plan's attribute changes, and for each input one setting
   sets directly, what exists next to what the round planned with.
3. **Plan again with the found values**, as often as needed: a new round with what the plan found filled in. What
   the module builds from several inputs (a NIC named `<name>-nic` by default) isn't found: **Change settings**
   opens the template's form with what you entered and what the plan found, to set it by hand; saving plans a
   new round. A run that changed several templates (a promotion) lists one *Change settings* link per template.
4. **Apply the import** when the latest round imports and replaces or deletes nothing (best when it only
   imports: `N to import, 0 to add, 0 to change, 0 to destroy`). It queues an apply run of exactly that round's
   configuration, which always waits for your confirmation, also with auto-apply. Once applied, the module stops
   importing: from then on it's an ordinary template in the workspace (a resource deleted outside Terraform
   later is created again, not looked for), and a promotion never copies the import.

Safety: a resource can be in one state only. A round or apply whose plan imports a resource a workspace manages
already (this one's state as the plan read it, or another's) ends as errored; the apply checks again when it
starts, in case another workspace imported it since the round. An apply that would replace or delete anything is
refused; a round that would can't be applied. Sensitive values are never filled in: type one (if the module
ignores it, as for a VM's admin password, any value works; if not, the plan shows it changes).

## Promote to another workspace

**Promote to…** in a workspace's menu (read access) makes another workspace, existing (operate) or new, an exact copy of its templates, e.g. dev's in test. A preview shows every change first, and the target's run always waits for confirmation. See [Environments and promotion]({{< relref "/docs/features/environments#promote-a-workspace" >}}).

## Update to a new module version

When a template is moved to a newer module version, its group on the workspace's **Resources** tab shows *update: <version>*. **Configure…** shows the form filled with its inputs, plus any new ones. Saving moves it to the template's version and queues a plan, which is confirmed as usual.

Every change to a workspace's templates (adding, configuring, removing, a rollout, a promotion) is a new **configuration version**, and its run plans exactly that version, in both phases. While the run is active the workspace shows the templates it's getting. The workspace takes the version when the run's apply starts (or when its plan has nothing to change). If the plan is discarded, or is canceled or errors before its apply starts, nothing changed: the workspace shows the templates it had, and an added template isn't there. Once the apply has started, the new version stays, even if the apply fails: the state may already hold part of it. A workspace created from a template starts with it, so discarding its first plan leaves the template to configure and plan again.

To update many workspaces at once, an admin opens **Roll out** on the template's card. The page lists each workspace that uses the template, with its module name and version. You can filter the list by project.

To start a rollout, pick the scope and a batch size, then click **Roll out**:

- **Scope:** the project selected in the filter, or all projects.
- **Batch size:** how many workspaces may have an open plan at a time. `0` means all at once.

The rollout moves each workspace to the template's version, with its current inputs and the defaults of new inputs, and queues one plan per workspace. These plans wait for confirmation even on auto-apply workspaces, so review each for replaced or destroyed resources. Every 30 seconds the rollout starts the next batch: as many workspaces as plans have ended.

- **Busy** workspaces (with an active run) are retried until that run ends.
- **Needs input:** the new version has a required input without a default. The rollout skips the workspace; use its update form.
- **Local execution:** runs can't start in Opsolete, so the rollout skips the workspace.

A rollout of its own that errors, is discarded or is canceled **pauses** the rollout: check it, then **Resume**. A workspace whose plan ended before its apply is back on its previous version, so resuming retries it; to skip it, **Stop** the rollout. **Stop** ends a rollout; plans it already queued stay. If the template moves to another version meanwhile, the rollout stops: start a new one. A rollout is **done** when no workspace in its scope is left to upgrade and its plans have ended. A template has at most one open rollout. Each upgrade is recorded in the audit log.

## Settings

A workspace's *Settings* need `operate`:

- **General:** its project (admins move it), **name** (state, runs and grants are keyed by its id and stay put), **execution mode** and **auto-apply**, and [remote state sharing]({{< relref "/docs/features/state#remote-state-sharing" >}}). *Remote* (the default) runs plans and applies on Opsolete's runners. *Local* is for the [Terraform CLI]({{< relref "/docs/features/terraform-cli" >}}): Terraform runs on your machine and Opsolete only keeps the state, so runs, drift checks, VCS triggers and rollouts don't start here. The mode can't change while a run is active (wait for it to end or cancel it); a CLI lock already under way isn't stopped by a change.
- **Health:** [drift detection]({{< relref "/docs/features/runs#drift-detection" >}}).
- **Locking:** lock the workspace's state by hand, with an optional reason; see [Locking]({{< relref "/docs/features/state#locking" >}}).
- **Team access:** the grants below (admins change them).
- **Destruction and deletion:** **Queue destroy plan** destroys the infrastructure (confirmed as usual). **Delete workspace** removes its runs, variables, grants and state files; it's refused while a run is active or the state is locked. Destroy the resources first: if its state still lists resources (or isn't indexed), the button says **Delete workspace anyway** (admins only), and any resources that still exist are no longer managed by Terraform and must be cleaned up by hand.

## Access

The creator and admins operate a workspace; IdP groups get `read` or `operate` per workspace or per [project]({{< relref "/docs/features/projects#access" >}}), and the higher of the two wins. Without access, a workspace returns 404, as if it didn't exist. A workspace's *Settings* need `operate`; with `read`, its Overview shows the project, repository link and trigger, execution mode, auto-apply and drift detection. See [Teams: access per workspace]({{< relref "/docs/config/authentication#teams-access-per-workspace" >}}).
