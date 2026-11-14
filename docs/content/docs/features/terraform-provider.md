---
title: Terraform provider
weight: 12
---

The [Opsolete provider](https://github.com/Opsolete/terraform-provider-opsolete) manages Opsolete's platform objects with Terraform: set up projects, their credentials and network values, and empty workspaces as code, and review changes to them in a plan like any other infrastructure.

| Managed with the provider | Stays in Opsolete's UI |
|---|---|
| Projects (name, description, runner pool, default) and their [environment values]({{< relref "/docs/features/environments#per-environment-inputs" >}}) | Templates, and templates on workspaces (adding one starts a plan) |
| Workspaces: created empty, name, project, auto-apply, execution mode, drift detection | Runs |
| Workspace variables, [variable sets]({{< relref "/docs/features/variables" >}}) and their variables, where sets apply | Team access |
| [VCS connections]({{< relref "/docs/features/vcs-providers" >}}) | States: their versions, rollbacks and locks |

It uses the [JSON API]({{< relref "/docs/endpoints#json-api" >}}), so the same access rules, validation and audit log apply as in the UI.

## Set it up

1. **A token:** an admin creates a service token under **Settings → API tokens**, or on the server with `opsolete api-token terraform` (to bootstrap: Terraform needs a token before it can create anything). It has admin rights and the audit log names it `token:terraform`. See [API tokens]({{< relref "/docs/config/authentication#api-tokens-automation" >}}).
2. **The provider:**

   ```hcl
   terraform {
     required_providers {
       opsolete = { source = "opsolete/opsolete" }
     }
   }

   # url and token, or leave both out and set OPSOLETE_URL and OPSOLETE_TOKEN
   provider "opsolete" {
     url   = "https://opsolete.example.com"
     token = var.opsolete_token
   }
   ```

{{< callout type="info" >}}
**Not on the Terraform Registry yet.** Until the first release, build it from source and point Terraform at the binary:

```sh
git clone https://github.com/Opsolete/terraform-provider-opsolete
cd terraform-provider-opsolete
go install .   # into $(go env GOPATH)/bin
```

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "opsolete/opsolete" = "/home/you/go/bin" # $(go env GOPATH)/bin
  }
  direct {}
}
```

With `dev_overrides`, skip `terraform init` for this provider: `plan` and `apply` use the binary directly (and warn that they do).
{{< /callout >}}

## Example: a project per subscription

One way to use projects, as code: a project per Azure subscription, its credentials and standard network in a variable set, its region as an environment value, and a workspace per resource group.

```hcl
resource "opsolete_project" "prd" {
  name        = "prd"
  description = "Production subscription"
  runner_pool = "prd" # only the prd runners get its runs, and its credentials
}

# The subscription's credentials and network: for all the project's workspaces
resource "opsolete_variable_set" "azure_prd" {
  name = "azure-prd"
}

resource "opsolete_variable_set_variable" "arm" {
  for_each = {
    ARM_TENANT_ID       = var.tenant_id
    ARM_SUBSCRIPTION_ID = var.subscription_id
    ARM_CLIENT_ID       = var.client_id
    ARM_CLIENT_SECRET   = var.client_secret
  }
  variable_set_id = opsolete_variable_set.azure_prd.id
  category        = "env"
  key             = each.key
  value           = each.value
  sensitive       = true
}

resource "opsolete_variable_set_variable" "vnet" {
  variable_set_id = opsolete_variable_set.azure_prd.id
  category        = "terraform"
  key             = "vnet_name"
  value           = "VNET"
}

resource "opsolete_variable_set_project" "azure_prd" {
  variable_set_id = opsolete_variable_set.azure_prd.id
  project_id      = opsolete_project.prd.id
}

# The subscription's region, for a template's per-environment input
resource "opsolete_environment_value" "region" {
  project_id  = opsolete_project.prd.id
  template_id = "storage-account" # the template's id in the catalog
  input       = "location"
  value       = "swedencentral"
}

# A workspace per resource group; its templates are added in Opsolete
resource "opsolete_workspace" "shop" {
  name            = "shop-prd"
  project_id      = opsolete_project.prd.id
  drift_detection = true
}
```

## Resources and data sources

| Resource | Manages | Import with |
|---|---|---|
| `opsolete_project` | name, `description`, `runner_pool`, `default` | its name or id |
| `opsolete_environment_value` | `project_id`, `template_id`, `input`, `value`, `keep_own` | `<project_id>/<template_id>/<input>` |
| `opsolete_workspace` | name, `project_id` (changing it moves the workspace), `auto_apply`, `execution_mode`, `drift_detection`, `force_delete` | its name or id |
| `opsolete_workspace_variable` | `workspace_id`, `category` (`env` or `terraform`), `key`, `value`, `sensitive`, `hcl` | `<workspace_id>/<variable_id>` |
| `opsolete_variable_set` | name, `description`, `global`, `priority` | its name or id |
| `opsolete_variable_set_variable` | as a workspace variable, with `variable_set_id` | `<variable_set_id>/<variable_id>` |
| `opsolete_variable_set_project` | applies a set to a project's workspaces | `<variable_set_id>/<project_id>` |
| `opsolete_variable_set_workspace` | applies a set to one workspace | `<variable_set_id>/<workspace_id>` |
| `opsolete_vcs_connection` | name, `provider_type` (`github` or `azure-devops`), `base_url`, `token`, `scope`, `project_ids`, `workspace_ids` | its name or id |

Data sources look up a `opsolete_project`, `opsolete_workspace` or `opsolete_variable_set` by name. Every attribute is described in the [provider's docs](https://github.com/Opsolete/terraform-provider-opsolete/tree/main/docs).

## Good to know

- **Sensitive values are write-only** (sensitive variables, VCS tokens): Opsolete never returns them, so the provider keeps the configured value and can't see a change made to it in the UI. They are in Terraform's state: keep that state encrypted (for instance in Opsolete itself).
- **A change in the UI to a managed object** is reverted by the next apply, as with any provider.
- **Renaming** a project or workspace changes it in place: it keeps its id, its state, runs and grants.
- **Deleting a workspace** whose state still manages resources fails, unless `force_delete = true` (admins): its resources then stay, unmanaged.
- **A token per automation**, so you can revoke one alone.
