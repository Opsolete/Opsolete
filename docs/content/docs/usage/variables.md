---
title: Variables and variable sets
weight: 4
---

A workspace's runs get **variables**: **env** (environment variables, e.g. cloud credentials) and **terraform** (Terraform input variables, passed as strings). Any runner runs any workspace; the server sends the variables with each claimed job.

- **Workspace variables:** the *Variables* section of a workspace page (operate access). A terraform variable can't share a name with one of the template's inputs: change those with *Configure*.
- **Variable sets** (admins, *Settings › Variable sets*): a named group of variables that's **global** (every workspace not linked to a repository) or attached to chosen workspaces and [projects]({{< relref "projects" >}}) (every workspace of the project not linked to a repository). Use one per subscription or service principal, e.g. `azure-prod` with `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, `ARM_CLIENT_ID` and a sensitive `ARM_CLIENT_SECRET`.
- **Precedence** (same category and key): template inputs > workspace variables > sets attached to the workspace > sets attached to its project > global sets. Between sets of the same kind, the first by name wins. A workspace's *Variables* page lists the sets that apply to it.

## Settings outside the template's form

A template's form asks only what users must choose, but a project can still set other module variables: a terraform variable in a set attached to the [project]({{< relref "projects" >}}) (or on the workspace) reaches every template whose module declares it, with the precedence above. The module declares the variable with a safe default, so other projects keep the standard. Added templates only get it from a [registry]({{< relref "/docs/catalog/module-registry" >}}) module (see [Add templates to a workspace]({{< relref "workspaces#add-templates-to-a-workspace" >}})).

Anyone who operates a workspace can set its terraform variables, so a variable the form doesn't show is not locked. Hard-code what must not change, or restrict the allowed values with a `validation` block.

### Maps, lists and objects

Terraform variables are passed as strings, and Terraform doesn't parse a string into a `map`, `list` or `object` variable: the run fails with `map of string required, but have string`. A variable of type `any` accepts it, but stays a string. Declare such a variable as a `string` holding JSON instead, and decode it in the module:

```hcl
variable "tags_json" {
  type    = string
  default = "{}"
  validation {
    condition     = can(tomap(jsondecode(var.tags_json)))
    error_message = "tags_json must be a JSON object of strings, e.g. {\"env\":\"prod\"}."
  }
}

locals {
  # the last map wins: a project adds tags, but can't change the standard ones
  tags = merge(tomap(jsondecode(var.tags_json)), { managed_by = "opsolete" })
}
```

Set it as a terraform variable with the value `{"env":"prod"}`. A list works the same way with `tolist(jsondecode(...))` and a value like `["10.0.0.1"]`. For an object, check the fields you need in the `validation` (e.g. `can(jsondecode(var.network_json).cidr)`), since a decoded object isn't type-checked.

## Security

{{< callout type="warning" >}}
**Repository-linked workspaces get no global or project sets**, only sets attached to the workspace itself. Any user who operates a workspace can link it to a branch, and Terraform runs arbitrary code during plan as well as apply (an `external` data source, a provider), so confirming a plan doesn't protect credentials.
{{< /callout >}}

An admin attaching a set to a VCS-linked workspace vouches for that repository and branch: changing either, switching to or changing tag-based triggers (a tag may point at any commit), or turning on pull request plans (they run unreviewed code), detaches all the workspace's sets. Anyone who can push to the branch can then use the credentials, so protect it (see [Which workspaces may link a connection]({{< relref "/docs/catalog/vcs-providers#which-workspaces-may-link-a-connection" >}})).

- **Sensitive** variables are write-only: the UI never shows them again, you can only replace or delete them. All values are AES-GCM encrypted with `SECRET_KEY` in Postgres. If `SECRET_KEY` changes, runs that need the old values fail with "can't decrypt": set them again.
- **Reserved env names:** `PATH`, `HOME`, `LD_*`, `DYLD_*`, `GIT_*`, `SSH_*`, `PYTHON*`, `OPS_*`, `TF_*` and a few more are refused, because they could run code on the runner or reconfigure Terraform. Use the terraform category instead of `TF_VAR_*`.
- **Who can read them:** module code and providers see the variables during the run. Anyone with operate access can add env variables, so treat operate access as "may use this workspace's credentials".

Provider credentials from variables are set as environment variables for Terraform on top of the runner's own environment. Runners are generic: give them no cloud credentials of their own, or every workspace without variables runs with them. Module code runs on the runner with the provider credentials, so scope them to what Opsolete may deploy.

## Upgrading from runner pools

Queued runs of workspaces that had a pool are errored by the migration. Move each pool's credentials into a variable set, attach it to those workspaces, remove the cloud identity from the runner Deployments, and start new runs.
