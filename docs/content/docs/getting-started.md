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

## Deploy

Every way to deploy runs the same two parts: the **app** (web UI, module registry, state backend, runner API) and one or more **runners** (they run Terraform and reach the app over HTTP(S), so they need no database access and no open ports). The app needs:

- PostgreSQL 13+.
- `SECRET_KEY`: at least 32 characters, e.g. `openssl rand -base64 32`. It encrypts state, plans and secrets: back it up, losing it loses all state.
- `RUNNER_TOKEN`: at least 32 characters, e.g. `openssl rand -hex 32`. Runners present it as `OPS_RUNNER_TOKEN`.
- `STATE_STORE`: where state files go, e.g. `azblob://tfstate`, `s3://tfstate?region=eu-west-1` or `gs://tfstate`.
- A login: OIDC (`OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`), and `ADMIN_EMAILS` or `ADMIN_GROUPS` for the first admin.

Every setting is listed under [Configuration]({{< relref "/docs/operations/configuration" >}}), and [Deployment]({{< relref "/docs/operations/deployment" >}}) covers each option below in more depth.

### Docker

The release images are `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner` (linux/amd64), tagged with the version (`0.6.0`, `0.6`), `latest` and the commit. To try it on one machine, with a throwaway database and state and no login:

```sh
docker network create opsolete
docker run -d --name opsolete-db --network opsolete -e POSTGRES_USER=opsolete -e POSTGRES_PASSWORD=opsolete postgres:17
export RUNNER_TOKEN=$(openssl rand -hex 32)
docker run -d --name opsolete-app --network opsolete -p 8080:8080 \
  -e DATABASE_URL='postgres://opsolete:opsolete@opsolete-db:5432/opsolete?sslmode=disable' \
  -e SECRET_KEY=$(openssl rand -base64 32) -e RUNNER_TOKEN \
  -e STATE_STORE='file:///tmp/state?create_dir=1&metadata=skip' \
  -e AUTH_DEV_USER=dev@localhost -e ADMIN_EMAILS=dev@localhost \
  ghcr.io/opsolete/opsolete-app:0.6
docker run -d --name opsolete-runner --network opsolete \
  -e OPS_SERVER_URL=http://opsolete-app:8080 -e OPS_RUNNER_TOKEN=$RUNNER_TOKEN \
  ghcr.io/opsolete/opsolete-runner:0.6
```

Then open http://opsolete.localhost:8080. This setup is for trying Opsolete only:

- `AUTH_DEV_USER` logs every visitor in as that user.
- The state in `/tmp` is lost with the container.
- `SECRET_KEY` changes on every run of the command.
- The images are linux/amd64 only. On an arm64 machine such as Apple Silicon, Docker runs them under emulation: the app works, but the runner exits (`harden runner process: PR_SET_CHILD_SUBREAPER`). Run the runner outside Docker there instead (see [Run locally](#run-locally-with-nix-recommended)), with `OPS_SERVER_URL=http://localhost:8080`.

For real use, keep a fixed `SECRET_KEY` and set `PUBLIC_URL`, OIDC and a cloud `STATE_STORE` instead (see [Containers]({{< relref "/docs/operations/deployment#containers" >}})). Run more runner containers for more jobs at once: each runs one job at a time.

### Kubernetes with Helm

The [Helm chart](https://github.com/Opsolete/Helm-charts) runs the app and the runners as two Deployments. Put the secrets in a Secret with the keys `DATABASE_URL`, `SECRET_KEY`, `RUNNER_TOKEN` and `OIDC_CLIENT_SECRET` (or manage it with sops or External Secrets):

```sh
kubectl create secret generic opsolete \
  --from-literal=DATABASE_URL=postgres://opsolete:…@postgres:5432/opsolete \
  --from-literal=SECRET_KEY=$(openssl rand -base64 32) \
  --from-literal=RUNNER_TOKEN=$(openssl rand -hex 32) \
  --from-literal=OIDC_CLIENT_SECRET=…
```

Then a values file:

```yaml
# my-values.yaml
config:
  PUBLIC_URL: https://opsolete.example.com
  STATE_STORE: azblob://tfstate
  AZURE_STORAGE_ACCOUNT: mystorageaccount
  OIDC_ISSUER: https://login.example.com
  OIDC_CLIENT_ID: opsolete
  ADMIN_EMAILS: you@example.com
secrets:
  existingSecret: opsolete
ingress:
  enabled: true # the host comes from PUBLIC_URL
```

```sh
helm repo add opsolete https://opsolete.github.io/Helm-charts
helm install opsolete opsolete/opsolete -f my-values.yaml
# or, as an OCI artifact:
helm install opsolete oci://ghcr.io/opsolete/charts/opsolete -f my-values.yaml
```

The chart's [values.yaml](https://github.com/Opsolete/Helm-charts/blob/main/charts/opsolete/values.yaml) lists every option. For example, `runner.replicas` sets how many jobs run at once, and `workloadIdentity` gives the app access to `STATE_STORE` without static keys. See [Kubernetes]({{< relref "/docs/operations/deployment#kubernetes" >}}).

### NixOS

Add this repository as a flake input and import its module. It creates a local PostgreSQL database by default, and passes secrets in from files (here from [sops-nix](https://github.com/Mic92/sops-nix)), so they never reach the Nix store:

```nix
# flake.nix
{
  inputs.opsolete.url = "github:Opsolete/Opsolete";
  outputs = { nixpkgs, opsolete, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [ opsolete.nixosModules.default ./configuration.nix ];
    };
  };
}
```

```nix
# configuration.nix
services.opsolete = {
  enable = true;
  publicUrl = "https://opsolete.example.com";
  secretKeyFile = config.sops.secrets.opsolete-secret-key.path;
  runnerTokenFile = config.sops.secrets.opsolete-runner-token.path;
  stateStore = "azblob://tfstate"; # default: files under /var/lib/opsolete
  oidc = { issuer = "https://login.example.com"; clientId = "opsolete"; clientSecretFile = config.sops.secrets.opsolete-oidc.path; };
  adminEmails = [ "you@example.com" ];
  runner.enable = true; # a runner on the same host; terraform is unfree (allowUnfree), or set runner.terraformPackage = pkgs.opentofu
};
```

The server listens on `127.0.0.1:8080`: put a reverse proxy with TLS in front of it, for example nginx, at `publicUrl`. `sudo opsolete-admin <subcommand>` runs any command with the server's settings, for example `token <name>` for a registry token. See [NixOS]({{< relref "/docs/operations/deployment#nixos" >}}) for runners on other hosts and every option.

## Examples

`just seed` loads the examples through the real endpoints and a local runner: the registry module `examples/greeting/terraform` (from `examples/modules/greeting/`) in versions 1.0.0 and 1.1.0, the `example-azure` variable set, the `greeting` template (`catalog/greeting.json`), and workspaces of every kind:

- `greeting-dev`: applied, auto-apply.
- `greeting-prod`: plan waiting for confirmation, with the `example-azure` variable set.
- `empty` and `credentials-only`: only variables.

The module uses Terraform's built-in `terraform_data`, so a local runner applies it without cloud credentials or provider downloads. All credentials in the examples are fake. `seed` expects an empty database, so use `just reseed` to start over.
