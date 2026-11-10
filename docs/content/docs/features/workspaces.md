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

A workspace still has one active run at a time, so VCS runs wait their turn; if several commits land meanwhile, only the newest gets a run. Both settings are on the workspace's **Version control** page, which also shows whether the link works (**Connected to VCS**: the connection allows the workspace and the branch or tags can be read).

**Tag-based** triggers replace both: a new tag in a chosen format (semantic version `1.2.3`, with a prefix `v1.2.3`, with a suffix `1.2.3-rc`, or a custom regular expression) queues an apply run of its commit, manual runs plan the newest matching tag (by the version number in its name; tags without one are never picked), and pull requests get no plans.

## Add templates to a workspace

Add any number of templates (**Deploy…** in a workspace's menu, operate access). Each becomes a module block, `module.<name>`, next to the workspace's own configuration (its branch, or nothing), in the same state; add one template several times under different names.

- Adding, configuring or removing one starts a plan; removing one plans the destruction of its resources. While the workspace's state manages no resources, it's removed at once, without a run.
- Removing the last one (the workspace's only configuration) starts a destroy run instead, and the template goes once that destroy is applied.
- Their outputs show as `<name>__<output>`. Terraform keeps all outputs after a destroy limited to one template, so once that destroy is applied, the same run plans the configuration without the template (`-refresh=false`) and applies that plan if it changes only outputs. Otherwise the log says so, and the next apply removes them.
- Workspace variables and variable sets reach every template: env variables (credentials) as the environment, Terraform variables as the variables of a linked branch's configuration and as inputs of each template whose module declares them and whose form doesn't ask for them (inputs the form asks keep their value, and a hidden or empty one the module's default). Templates on external sources (e.g. `git::`) get no Terraform variables, only environment variables: publish their module to the registry (it can sync from VCS). HCP Terraform's no-code provisioning has the same rule: only private registry modules.
  - *Possible improvement:* the runner has every template's module on disk after `terraform init`, `git::` ones too. It could add the workspace Terraform variables each module declares itself, if the job also lists the inputs each template's form asks for, so those keep winning.
- Each template's module configures its own providers (see [Writing modules for templates]({{< relref "/docs/features/templates#writing-modules-for-templates" >}})).

## Import existing resources

Resources built outside Opsolete (in the Azure portal, by scripts) can be taken over without being created again:
**Import…** in a workspace's menu (operate access).

1. **Scan**, optionally in one resource group. A scan is a run (action `scan`) with the workspace's credentials that
   lists every resource in the subscription (or the resource group), and changes nothing: no state, no lock, no apply.
2. The scan's page sorts them into four groups. Resources one imports or uses (a VM's network interface, its OS
   disk) are listed with it.
   - **Can be imported**: a catalog template can manage it. Each was planned as if imported, into a throwaway state:
     **fits** (imported as it is, nothing changes), **differs** (the plan would also change the attributes listed:
     the template's defaults differ from the resource), **incomplete** (values the form requires weren't found; fill
     them in when importing).
   - **Template needs work**: a template matches, but importing with it would replace the resource; the attributes
     that force it say what its module must make an input.
   - **No template**: no catalog template can import its type yet.
   - **Already managed**: a workspace's state has it, with a link to that workspace.
3. **Import** opens the template's form filled in from what was found. Check it and start the plan: it imports the
   resources (`N to import, 0 to add, 0 to change, 0 to destroy` when it fits) and always waits for confirmation,
   also with auto-apply. From then on it's an ordinary template in the workspace.

What a template can find, and how that fills its form, is read from its module's code: templates need nothing for
it, but their module must be in the [registry]({{< relref "/docs/features/module-registry" >}}). Each managed
resource of the module is searched by its type (`terraform query`, azurerm ≥ 5.7 for VMs); `count = var.x == "v" ?
1 : 0` makes it exist for that value of `x`; `attr = var.x` and `coalesce(var.x, …)`, also in nested blocks, fill
input `x`; a `dynamic` block with `for_each = var.x ? … : []` fills the yes / no input `x`; `[other.res.id]` makes
`other` a related resource; a sensitive input in `lifecycle { ignore_changes }` gets a random placeholder. Module
calls are followed: local ones (`./x`) and ones from the registry (its newest version the constraint allows), with
values through arguments, `locals` and outputs. Optional and repeated parts (a module call with
`count = var.x != null ? 1 : 0` or `for_each`: a VM's data disks, its auto-shutdown schedule, its security group)
aren't searched, as Terraform can't list most of them: fill them in on the form, and a module that imports them
itself (an `existing` input, see the link below) takes the existing ones over too; the fit check shows any value that differs. Other
expressions are left for the form; the scan's log names what was skipped. See
[Writing modules that can import]({{< relref "/docs/features/templates#writing-modules-that-can-import" >}}).

Safety: a resource can be in one state only. Importing one a workspace manages already is refused, and the plan
checks again against the state it reads; an import whose plan would replace or delete anything ends as errored,
nothing applied. The IDs to import come from the scan, never from the form.

Limits: listing every resource is Azure's (`azurerm_resources`, per resource group); without it the page shows only
what templates can import. At most 50 resources per scan get a fit check (the rest are listed unchecked); types the
provider can't list (e.g. managed disks) are imported by ID only, their inputs not filled.

## Promote to another workspace

**Promote to…** in a workspace's menu (read access) makes another workspace, existing (operate) or new, an exact copy of its templates, e.g. dev's in test. A preview shows every change first, and the target's run always waits for confirmation. See [Environments and promotion]({{< relref "/docs/features/environments#promote-a-workspace" >}}).

## Update to a new module version

When a template is moved to a newer module version, the workspace's **Templates** tab shows *update: <version>* next to it. **Configure…** shows the form filled with its inputs, plus any new ones. Saving moves it to the template's version and queues a plan, which is confirmed as usual.

If that plan is discarded, or is canceled or errors before its apply starts, the template goes back to its previous version and inputs, so it never shows a version that wasn't applied. The same holds for adding a template (it's removed again) and for rollouts. Once the apply has started, the new version stays, even if the apply fails: the state may already hold part of it.

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
- **Destruction and deletion:** **Queue destroy plan** destroys the infrastructure (confirmed as usual). **Delete workspace** removes its runs, variables, grants and state files; it's refused while a run is active or the state is locked. Destroy the resources first: if its state still lists resources (or isn't indexed), the button says **Delete workspace anyway**, and any resources that still exist are no longer managed by Terraform and must be cleaned up by hand.

## Access

The creator and admins operate a workspace; IdP groups get `read` or `operate` per workspace or per [project]({{< relref "/docs/features/projects#access" >}}), and the higher of the two wins. Without access, a workspace returns 404, as if it didn't exist. A workspace's *Settings* need `operate`; with `read`, its Overview shows the project, repository link and trigger, execution mode, auto-apply and drift detection. See [Teams: access per workspace]({{< relref "/docs/config/authentication#teams-access-per-workspace" >}}).
