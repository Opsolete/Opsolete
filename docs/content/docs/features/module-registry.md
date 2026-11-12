---
title: Module registry
weight: 10
aliases:
  - /docs/catalog/module-registry/
---

Opsolete is a private Terraform module registry, built on the standard [module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol): `/.well-known/terraform.json`, `GET /v1/modules/:ns/:name/:system/versions` and `.../:version/download`. HashiCorp's `terraform-config-inspect` reads each uploaded module's variables, `terraform-registry-address` validates addresses and `go-version` handles semver.

- **Upload** (once an admin turns on *Module uploads* in **Settings → Organization**): on `/registry`, *Publish module* → *Upload workflow*: pick a namespace, name, system and semver version, plus a `.tar.gz` with the module at its root (`tar czf module.tar.gz -C path/to/module .`). Versions are immutable.
- **The list** (`/registry`) is for finding a module: its name, namespace and system, latest version and when it was published; a *VCS* tag (admins) marks one published from a repository, *sync failed* its last sync's error. What to do with a module is on its page: *Create template*, *Sync now* and *Unlink* for one published from a repository, *Delete module*.
- **Delete** (admins): *Delete module* on a module's page removes all its versions and unlinks it from its repository. It's refused while a template still uses the module. Workspaces that already use it keep their state, but their next run can't download it.
- **Publish from Git:** see [VCS providers]({{< relref "/docs/features/vcs-providers" >}}).
- **Docs:** each upload gets generated documentation at `/registry/<ns>/<name>/<system>/<version>`, similar to `terraform-docs`. It includes a usage snippet with the required inputs, Terraform and provider requirements, inputs (type, default, required), outputs, resources, data sources, module calls and the archive's `README.md` shown as plain text. A **Markdown** link (`.../docs.md`) serves HashiCorp's `tfconfig.RenderMarkdown` output to paste into a repo.
- **Downloads:** `download` returns a 10-minute HMAC-signed archive URL, because Terraform doesn't send the token to archive URLs. Runners get a fresh 15-minute signed path with each job. The signing key is kept in Postgres.

## Use from Terraform

```sh
just token my-laptop     # or `opsolete token <name>`; printed once
export TF_TOKEN_opsolete_example_com=<token>
```

```hcl
module "x" {
  source  = "opsolete.example.com/platform/storage/azurerm"
  version = "1.0.0"
}
```

Tokens from [`terraform login`]({{< relref "/docs/features/terraform-cli" >}}) work too.

**Modules that call other registry modules** (a VM module composed of a network interface, data disk and so on, each its own module) work in runs too: every job gets a read-only registry token for its run, valid only while a runner works on it, which the runner hands Terraform as a `credentials` block for this registry's host (on top of a CLI config file the runner already has, `TF_CLI_CONFIG_FILE`). A `host` block next to it points the host's module service at the runner's own server URL (`OPS_SERVER_URL`), so Terraform skips service discovery: runs fetch nested registry modules over plain HTTP in local development too, and runners needn't reach the public URL. Terraform only talks to registries over HTTPS on a hostname with a dot, so put the app behind TLS in production.
