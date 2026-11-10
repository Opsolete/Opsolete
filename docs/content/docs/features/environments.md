---
title: Environments and promotion
weight: 5
aliases:
  - /docs/usage/environments/
---

Opsolete has no separate "environment" object: **projects are the environments**. Put dev, test, acc and prd each in their own [project]({{< relref "/docs/features/projects" >}}), build in dev, and **promote** a workspace to the next environment instead of filling in its forms again.

## Model environments as projects

- **One project per environment**, e.g. `dev`, `test`, `acc`, `prd`, and one workspace per application per environment (`shop-dev`, `shop-test`, …). Workspace names are unique across projects, so give each its environment in the name.
- **Credentials:** attach a [variable set]({{< relref "/docs/features/variables" >}}) per project (the dev subscription's `ARM_CLIENT_ID` … to `dev`, prd's to `prd`). Every workspace in the project gets it at each run, except repository-linked ones (see [Variable sets]({{< relref "/docs/features/projects#variable-sets" >}})).
- **Values that differ per environment** (a region, a size, a network range): mark those template inputs [per environment](#per-environment-inputs) and set them once per project.
- **Access:** grant groups `operate` on the projects they deploy to and `read` elsewhere ([Access]({{< relref "/docs/features/projects#access" >}})). Who may promote into an environment follows from that.

## Per-environment inputs

A template author marks an input `"per_environment": true` ([template format]({{< relref "/docs/features/templates#template-format" >}}); the builder's **Per environment** switch). Then each project may set one value for that input, for all its workspaces:

- **Who sets them:** admins, and groups with `operate` on the project (whoever may create workspaces there), on the project's **Environment values** tab. Everyone who sees the project reads them there. Each value shows who set it and when; **Clear** removes it.
- **The value is checked** by the input's own rules (type, options, pattern) when it's set. It can't be empty: clear it instead.
- **It applies at every run, like a variable set:** the server puts it in when a runner claims the job, from the workspace's project at that moment, over the workspace's own value. Changing it changes nothing at once; the next plan of each workspace shows the change. A value the template's current rules no longer accept fails the job (rather than deploying the workspace's own value): set it again.
- **Read-only in forms:** the deploy and configure forms show such an input as its value with *Set by project X*; a value posted for it is ignored, and the workspace keeps its own stored value underneath. On the deploy form, picking another project shows the form again with that project's values, keeping what you typed. [Rollouts]({{< relref "/docs/features/workspaces#update-to-a-new-module-version" >}}) count the project's value too, so such an input doesn't make a workspace *need input*.
- **Without a project value** the input behaves like any other: the workspace's form sets it.
- **Moving a workspace** to another project switches its values: the next run uses the new project's.
- **Not for sensitive or key inputs:** project values are stored and shown in plain text, and the key names the module once. A module variable declared `sensitive` stays a per-workspace input even if the template marks it per environment.
- A hidden input (its [`show_if`]({{< relref "/docs/features/templates#conditional-inputs" >}}) doesn't match) gets no project value, as the form wouldn't send it.

## Promote a workspace

**Promote to…** in a workspace's menu makes another workspace (the target) an **exact copy of this workspace's templates** (the source's).

### Who may promote

- **Read** on the source.
- An **existing target:** `operate` on it. The picker lists the remote-execution workspaces you operate. The source itself and a workspace using local execution can't be targets.
- A **new target:** a name and a project you may [create workspaces in]({{< relref "/docs/features/projects#access" >}}). The workspace is created by the promotion, in that project, with remote execution.

### What is copied

| | |
|---|---|
| Templates: module names, template, module source and version | copied, exactly |
| Non-sensitive inputs | copied |
| Per-environment inputs | **not copied:** the target's project value applies at every run; without one, the target keeps its own value; without that, the promotion asks for it |
| Sensitive inputs | **never copied:** the target's own value for the same template and input is kept; without one, the promotion asks for it |
| VCS link, variables, variable sets, settings (auto-apply, drift detection, locking, access) | not promoted: they belong to the environment |

**Exact copy:** templates only the target has are **removed**, and their resources destroyed. A module name that holds a different template in the target is replaced: removed, then added. Inputs are checked against the version the source runs; a copied value that the template rejects (and no field can fix) blocks the promotion until it's fixed in the source. A source without templates, or with a template no longer in the catalog, can't be promoted.

### The preview

Picking a target shows what changes there before anything does, per module: *added*, *updated* (version `1.0.0 → 1.1.0`, each changed input `old → new`), *replaced*, *removed* (with the number of resources it destroys) or *unchanged*; per-environment inputs with where their value comes from (*set by project X*, or the target's own), sensitive inputs kept. Values the promotion needs are asked for in a form below it. If nothing would change, there's nothing to promote.

The preview is pinned: if the source's or the target's templates change before you promote, the promotion is refused and you check the preview again. It's refused too while the target has an active run.

### One run, or two

**Promote** queues a plan on the target and opens it. **Promotion runs always wait for confirmation**, even on an auto-apply workspace: review the plan, then confirm or discard it.

- **One run** (the usual case): an apply run with every change, removals included.
- **Two chained runs** when the promotion removes or replaces a template that is deployed (its resources are in the target's state). A template's module [configures its own providers]({{< relref "/docs/features/templates#writing-modules-for-templates" >}}), and Terraform can't destroy resources once the module block that configures their provider is gone. So the first run is a **destroy** targeting only the removed and replaced modules; it changes nothing else. Once it is applied, the promotion's **apply run** is queued automatically and waits for confirmation too. The preview says *This promotion runs twice*.

### Discarding or failing

| | What the target keeps |
|---|---|
| One run discarded, or its plan fails or is canceled | its previous templates, removed ones included (unless a template took that module name meanwhile) |
| One run, the apply started (even if it fails) | the promoted templates: the state may already hold part of them |
| Chained: the destroy run discarded, failed or canceled | its templates as before (a destroy that failed while applying may have destroyed some resources); the promotion is dropped (the run's history says *promotion not continued*). Promote again |
| Chained: the destroy applied, the apply run discarded or its plan failed | the templates after the destroy: the removed ones stay removed, the rest as before the promotion |
| A new target, its run discarded or failed | the new workspace, without templates |

### Audit

Every promotion is in the [audit log]({{< relref "/docs/config/authentication#audit-log" >}}) as `promote` with the source, target, whether the target is new, the run and whether it is chained. A chained promotion's apply run adds `promote apply queued`, in the promoter's name, when the destroy is applied.

## Example: dev → test

1. Projects `dev` and `test`, each with a variable set holding its own cloud credentials. The template *Storage account* marks `location` per environment; **Environment values** on `dev` sets it to `westeurope`, on `test` to `northeurope`.
2. In `shop-dev` (project `dev`) you deploy *Storage account* and *Key vault*, and test them.
3. On `shop-dev`, **Promote to…** → *A new workspace*: name `shop-test`, project `test` → **Preview**. Both templates are *added*; `location` shows `northeurope`, *set by project test*; the key vault's sensitive `admin_password` needs a value, since dev's is never copied. Fill it in, **Promote & start plan**.
4. `shop-test`'s plan creates both, in `northeurope`, with test's credentials. Confirm it.
5. Later dev moves *Storage account* to `1.1.0` and drops *Key vault*. Promoting again to `shop-test` previews *updated* (`1.0.0 → 1.1.0`) and *removed* (with its resources). Because the key vault is deployed, it runs twice: confirm the destroy, then the apply run that follows.

## Limits

- **No promotion paths:** the target is picked each time; nothing enforces dev → test → acc → prd.
- **Templates only:** promotion copies a workspace's templates. A workspace's own configuration from a linked repository branch isn't promoted; promote that through your branches or tags.
- Promoting to a workspace that uses local execution isn't possible: its runs don't start in Opsolete.
