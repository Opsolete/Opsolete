---
title: Getting started
weight: 1
---

## Run locally with Nix (recommended)

```sh
nix develop        # or `direnv allow`; gives go, gopls, sqlc, postgres 17, terraform, infracost, just, hugo
just               # list tasks
just run           # start project-local postgres (.devdata/, unix socket) + app on :8080
just runner        # in a 2nd terminal: a runner on this machine (state goes to .devdata/state-store)
just reseed        # wipe the dev DB + state files (stops postgres, so not while `just run` runs), then `just seed`
just test          # sqlc diff, gofmt check, vet, all tests incl. DB tests
just test-linux    # the non-DB tests on Linux in docker (go.mod's Go, CI's Terraform)
just smoke         # nix build, boot the binary, create a workspace from a template and list it (no runner)
just check         # test + smoke, run before committing
just token <name>  # registry token for Terraform clients
just image app|runner   # container images, built with nix (also ghcr.io/opsolete/opsolete-app|runner)
just docs          # preview this documentation on http://localhost:1313/Opsolete/
just sqlc          # regenerate internal/storage/db after changing internal/storage/queries
just psql | db-start | db-stop | db-reset | down
```

`nix build` only sees files tracked by git, so `git add` new files first. After changing dependencies in `go.mod`, `just build` (also run by `just smoke` and `just check`) updates `vendorHash` in `flake.nix`; commit that change along with `go.mod`.

## Run locally without Nix

Requirements: Go 1.26+, PostgreSQL 13+ (it needs `gen_random_uuid()`; CI tests 17), Terraform for a runner.

```sh
docker compose up -d            # compose.yaml: PostgreSQL 17 on 127.0.0.1:5432; or use any local PostgreSQL
export DATABASE_URL=postgres://opsolete:opsolete@localhost:5432/opsolete
export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost   # local only, see Login
export SECRET_KEY=$(openssl rand -hex 32) RUNNER_TOKEN=$(openssl rand -hex 32) STATE_STORE="file://$PWD/.devdata/state-store?create_dir=1&metadata=skip"
go run ./cmd/opsolete
```

Then open http://localhost:8080. The schema is created on startup. For a runner, in a second terminal with the same `RUNNER_TOKEN`: `OPS_RUNNER_TOKEN=$RUNNER_TOKEN OPS_SERVER_URL=http://localhost:8080 go run ./cmd/opsolete runner`. All settings are listed under [Configuration]({{< relref "/docs/operations/configuration" >}}); `AUTH_DEV_USER` is explained under [Login]({{< relref "/docs/operations/authentication" >}}).

## Examples

`just seed` loads the examples through the real endpoints and a local runner: the registry module `examples/greeting/terraform` (from `examples/modules/greeting/`) in versions 1.0.0 and 1.1.0, the `example-azure` variable set, the `greeting` template (`catalog/greeting.json`), and workspaces of every kind:

- `greeting-dev`: applied, auto-apply.
- `greeting-prod`: plan waiting for confirmation, with the `example-azure` variable set.
- `empty` and `credentials-only`: only variables.

The module uses Terraform's built-in `terraform_data`, so a local runner applies it without cloud credentials or provider downloads. All credentials in the examples are fake. `seed` expects an empty database, so use `just reseed` to start over.
