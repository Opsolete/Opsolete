---
title: Getting started
weight: 1
---

## Run locally with Nix (recommended)

```sh
nix develop        # or `direnv allow`; gives go, gopls, postgres 17, terraform, just, hugo
just               # list tasks
just run           # start project-local postgres (.devdata/, unix socket) + app on :8080
just runner        # in a 2nd terminal: a runner on this machine (state goes to .devdata/state-store)
just reseed        # wipe the dev DB + state files, then `just seed`: example data via the real endpoints
just test          # gofmt check, vet, all tests incl. DB tests
just smoke         # nix build, boot the binary, create a workspace end-to-end
just check         # test + smoke, run before committing
just token <name>  # registry token for Terraform clients
just image app|runner   # container images, built with nix (also ghcr.io/opsolete/opsolete-app|runner)
just docs          # preview this documentation on http://localhost:1313/Opsolete/
just psql | db-reset | down
```

`nix build` only sees files tracked by git, so `git add` new files first. After changing dependencies in `go.mod`, update `vendorHash` in `flake.nix` (set it to `nixpkgs.lib.fakeHash`, run `nix build`, and copy the hash it prints).

## Run locally without Nix

Requirements: Go 1.26+, PostgreSQL 13+ (it needs `gen_random_uuid()`).

```sh
docker compose up -d            # or use any local PostgreSQL
export DATABASE_URL=postgres://opsolete:opsolete@localhost:5432/opsolete
export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost   # local only, see Login
export SECRET_KEY=$(openssl rand -hex 32) RUNNER_TOKEN=$(openssl rand -hex 32) STATE_STORE="file://$PWD/.devdata/state-store?create_dir=1&metadata=skip"
go run ./cmd/opsolete
```

Then open http://localhost:8080. Migrations run on startup. All settings are listed under [Configuration]({{< relref "/docs/operations/configuration" >}}); `AUTH_DEV_USER` is explained under [Login]({{< relref "/docs/operations/authentication" >}}).

## Examples

`just seed` loads the examples from `examples/`: the registry module `examples/greeting/terraform` in versions 1.0.0 and 1.1.0, the `greeting` template (`catalog/greeting.json`), and workspaces of every kind:

- `greeting-dev`: applied, auto-apply.
- `greeting-prod`: plan waiting for confirmation, with the `example-azure` variable set.
- `empty` and `credentials-only`: only variables.

The module uses Terraform's built-in `terraform_data`, so a local runner applies it without cloud credentials or provider downloads. All credentials in the examples are fake. `seed` expects an empty database, so use `just reseed` to start over.
