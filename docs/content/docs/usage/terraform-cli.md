---
title: Terraform CLI
weight: 5
---

Run Terraform on your own machine with Opsolete as the state backend, using Terraform's [`cloud` block](https://developer.hashicorp.com/terraform/cli/cloud/settings) as for HCP Terraform:

```sh
terraform login opsolete.example.com     # browser: confirm "Authorize Terraform CLI"; the token is saved in ~/.terraform.d
```

```hcl
terraform {
  cloud {
    hostname     = "opsolete.example.com"
    organization = "opsolete"            # ORGANIZATION on the server
    workspaces { name = "network-dev" }
  }
}
```

- **Local execution only.** `terraform plan` and `apply` run on your machine with your credentials; Opsolete doesn't run them on its runners. Workspace variables and variable sets aren't used.
- **Workspaces:** `terraform init` creates the named workspace if it doesn't exist, as a blank workspace owned by you. An existing one needs **operate** access (read access is a 404 for the state, which holds secrets). Only `workspaces { name = … }` works: tags and projects aren't supported.
- **State** works like a run's: every save is a state version with outputs (sensitive ones masked) and a resource list, rollback works, and the CLI takes the same state lock as runs, so a run and a CLI never write at the same time. `terraform force-unlock` only releases locks held by a CLI, not by a run.
- **Tokens** from `terraform login` act as you (your role and groups from your last browser login) and expire after 30 days: run `terraform login` again. They also work for the [module registry]({{< relref "/docs/catalog/module-registry" >}}) on the same host. There's no page to revoke them yet: `DELETE FROM api_tokens WHERE scope = 'user'` revokes them all.

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
