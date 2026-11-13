---
title: Variables and variable sets
weight: 4
aliases:
  - /docs/usage/variables/
---

A workspace's runs get **variables**: **env** (environment variables, e.g. cloud credentials) and **terraform** (Terraform input variables, passed as strings, or as typed values when marked **HCL**). Any runner runs any workspace; the server sends the variables with each claimed job.

- **Workspace variables:** a workspace's *Variables* page (anyone with read access sees them, sensitive values never; setting and deleting needs operate).
- **Variable sets** (admins, *Settings › Variable sets*): a named group of variables that's **global** (every workspace not linked to a repository) or attached to chosen workspaces and [projects]({{< relref "/docs/features/projects" >}}) (every workspace of the project not linked to a repository). Use one per subscription or service principal, e.g. `azure-prod` with `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, `ARM_CLIENT_ID` and a sensitive `ARM_CLIENT_SECRET`.
- **Precedence** (same category and key): template inputs > *priority* sets (values an admin enforces; wherever the set applies) > workspace variables > sets attached to the workspace > sets attached to its project > global sets. Between sets of the same kind, the first by name wins. A workspace's *Variables* page lists the sets that apply to it.

## Settings outside the template's form

A template's form asks only what users must choose; a project can set the rest once. A **terraform variable** of the workspace, or of a variable set attached to its [project]({{< relref "/docs/features/projects" >}}) (or global, or attached to the workspace), fills every template input of that name that the **template's JSON doesn't list**, with the precedence above. Example: a subscription's standard network, `vnet_name = VNET` in a set attached to the project, for every template whose module declares `vnet_name`.

- **The template decides:** an input listed in the template's JSON (in the builder: an input card) is asked as always. Remove the card of a variable a set should fill. A registry module's other variables still show in the form when no variable fills them, required if the module has no default.
- **In the form**, a filled input shows its value read-only, *Set by a Terraform variable* (a sensitive one as `(sensitive)`); what's posted for it is ignored and nothing is stored, so changing the variable changes the next plan of every workspace using it. The deploy form shows the values of the picked project's sets.
- **An import** shows such an input's found value next to the variable's, but never asks for another round for it: change the variable instead.
- Only templates on [registry]({{< relref "/docs/features/module-registry" >}}) modules (see [Add templates to a workspace]({{< relref "/docs/features/workspaces#add-templates-to-a-workspace" >}})). For a value that differs per project but should show as the project's, use a [per-environment input]({{< relref "/docs/features/environments#per-environment-inputs" >}}) instead.

Anyone who operates a workspace can set its terraform variables, so a variable the form doesn't show is not locked. Hard-code what must not change, or restrict the allowed values with a `validation` block.

### Maps, lists and objects

Tick **HCL** and set the value as HCL: `{env = "prd"}` for a `map`, `["10.0.0.1"]` for a `list`, an object the same way; Terraform checks it against the variable's type. Without **HCL** the value is a string, which Terraform doesn't parse into a `map`, `list` or `object` (`map of string required, but have string`).

An alternative that works without HCL: declare the variable as a `string` holding JSON and decode it in the module:

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

An admin attaching a set to a VCS-linked workspace vouches for that repository and branch: changing either, switching to or changing tag-based triggers (a tag may point at any commit), or turning on pull request plans (they run unreviewed code), detaches all the workspace's sets. Anyone who can push to the branch can then use the credentials, so protect it (see [Which workspaces may link a connection]({{< relref "/docs/features/vcs-providers#which-workspaces-may-link-a-connection" >}})).

- **HCL** (Terraform variables only): the value is an HCL value, `["a", "b"]`, `{env = "prd"}`,
  `3` or `true`, for variables of a list, map, object, number or bool type; without it the value is a string. It's
  checked when saved: a literal, no references or functions.
- **Workspace variables win over a repository's `*.auto.tfvars`**: runners pass them with `-var-file`.
- **Sensitive** variables are write-only: the UI never shows them again, you can only replace or delete them. All values are AES-GCM encrypted with `SECRET_KEY` in Postgres. Change `SECRET_KEY` only by [rotating it]({{< relref "/docs/config/secret-key#rotating-it" >}}): after just replacing it, runs that need the old values fail ("sealed with key …, which is neither SECRET_KEY nor SECRET_KEY_PREVIOUS") until you set them again.
- **Masking** (best-effort): values of sensitive variables and sensitive template inputs are hidden wherever Terraform may echo them. In run logs each occurrence shows as `***`. In outputs (the run page, the workspace's Outputs tab, state versions and their diffs) a string that contains one, also inside a list or map, is stored as `(sensitive)` as a whole, so a partly masked value doesn't leak its length or context; outputs Terraform marks sensitive are always `(sensitive)`. Limits: values shorter than 4 characters (not counting spaces) and inputs that aren't strings aren't masked; a multi-line value is recognised line by line; a value that's encoded (base64, JSON-escaped, hashed) or split up isn't recognised. Mark outputs `sensitive` in the module rather than relying on this.
- **Reserved env names:** `PATH`, `HOME`, `SHELL`, `TMPDIR`, `NODE_OPTIONS`, `LD_*`, `DYLD_*`, `GIT_*`, `SSH_*`, `PYTHON*`, `OPS_*`, `TF_*` and a few more shell settings are refused, because they could run code on the runner or reconfigure Terraform. Use the terraform category instead of `TF_VAR_*`. The one exception is `OPS_DEBUG`: set it to `1` and the workspace's runs show Terraform's warnings in full. By default each warning is a single line (`-compact-warnings`).
- **Who can read them:** module code and providers see the variables during the run. Anyone with operate access can add env variables, so treat operate access as "may use this workspace's credentials".

Provider credentials from variables are set as environment variables for Terraform on top of the runner's own environment. Runners are generic: give them no cloud credentials of their own, or every workspace without variables runs with them. Module code runs on the runner with the provider credentials, so scope them to what Opsolete may deploy.
