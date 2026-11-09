---
title: Terraform CLI
weight: 6
---

Run Terraform on your own machine with Opsolete as the state backend, using Terraform's [`cloud` block](https://developer.hashicorp.com/terraform/cli/cloud/settings):

```sh
terraform login opsolete.example.com     # browser: confirm "Authorize Terraform CLI"; the token is saved in ~/.terraform.d
```

```hcl
terraform {
  cloud {
    hostname     = "opsolete.example.com"
    organization = "opsolete"            # ORGANIZATION on the server
    workspaces {
      name    = "network-dev"
      project = "networking"             # optional; else the default project
    }
  }
}
```

- **Local execution only.** `terraform plan` and `apply` run on your machine with your credentials; Opsolete doesn't run them on its runners. Workspace variables and variable sets aren't used.
- **Workspaces:** `terraform init` creates the named workspace if it doesn't exist, as a blank workspace owned by you, with **local execution**. An existing one must use local execution too (*Settings › General*): for a remote one, locking and saving state are refused, since only its runs write its state. Using it needs **operate** access: with read access the state calls are `403` (the state holds secrets), without access the workspace is `404`. Only `workspaces { name = … }` works, optionally with `project`: tags aren't supported.
- **Projects:** `project` (or `TF_CLOUD_PROJECT`) is where `terraform init` creates a missing workspace: the default project if unset. Creating a workspace needs **operate** access to its project, the default one too (admins: any project); without it, `terraform init` fails with a 422 error. A missing project is created only for admins; others get an error. An existing workspace stays in its project, whatever the block says.
- **State** works like a run's: every save is a state version with outputs (sensitive ones, and those containing a sensitive variable's value, masked) and a resource list, rollback works, and the CLI takes the same state lock as runs, so a run and a CLI never write at the same time. `terraform force-unlock` releases a lock held by a CLI or set in the UI, not one held by a run or a rollback.
- **Tokens** from `terraform login` act as you (your role and groups from your last browser login) and expire after 30 days: run `terraform login` again. They also work for the [module registry]({{< relref "/docs/catalog/module-registry" >}}) on the same host. Revoke them under *Account settings › Tokens* (click your email at the top): one, e.g. from a lost laptop, or all of them.

## Local development over HTTP

Like the registry, Terraform only uses HTTPS on a hostname with a dot. For local dev over HTTP, override discovery in a CLI config file (`~/.terraformrc`):

```hcl
host "opsolete.localhost:8080" {
  services = {
    "tfe.v2"   = "http://opsolete.localhost:8080/api/v2/"
    "login.v1" = { client = "terraform-cli", grant_types = ["authz_code"], ports = [10000, 10010],
                   authz = "http://opsolete.localhost:8080/oauth/authorization", token = "http://opsolete.localhost:8080/oauth/token" }
  }
}
```
