---
title: Module registry
weight: 3
---

Opsolete is a private Terraform module registry, built on the standard [module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol): `/.well-known/terraform.json`, `GET /v1/modules/:ns/:name/:system/versions` and `.../:version/download`. HashiCorp's `terraform-config-inspect` reads each uploaded module's variables, `terraform-registry-address` validates addresses and `go-version` handles semver.

- **Upload** (once an admin turns on *Module uploads* in **Settings → Organization**): on `/registry`, *Publish module* → *Upload workflow*: pick a namespace, name, system and semver version, plus a `.tar.gz` with the module at its root (`tar czf module.tar.gz -C path/to/module .`). Versions are immutable.
- **Publish from Git:** see [VCS providers]({{< relref "vcs-providers" >}}).
- **Docs:** each upload gets generated documentation at `/registry/<ns>/<name>/<system>/<version>`, similar to `terraform-docs`. It includes a usage snippet with the required inputs, Terraform and provider requirements, inputs (type, default, required), outputs, resources, data sources, module calls and the archive's `README.md` shown as plain text. A **Markdown** link (`.../docs.md`) serves HashiCorp's `tfconfig.RenderMarkdown` output to paste into a repo. Versions uploaded before docs existed show a notice; upload a new version to get docs.
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

Tokens from [`terraform login`]({{< relref "/docs/usage/terraform-cli" >}}) work too. Terraform only talks to registries over HTTPS on a hostname with a dot, so put the app behind TLS in production.
