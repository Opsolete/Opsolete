---
title: Option reference
weight: 1
aliases:
  - /docs/operations/configuration/
---

The server and the runner are configured with environment variables; an invalid value stops them at startup with a message naming it. The server applies its database schema on startup (also `opsolete token`, `opsolete runner-token` and `opsolete rotate-key`), and refuses a database created by Opsolete 0.5 or older (see [Upgrading to 0.6]({{< relref "/docs/install/upgrading" >}})). The NixOS column is the option under `services.opsolete` ([NixOS]({{< relref "/docs/install/nixos" >}})); variables without one go in `environment` (or `secretFiles` for secrets).

## Server

| Env var | Default | Purpose | NixOS |
|---|---|---|---|
| `DATABASE_URL` | required | PostgreSQL connection string | `database.url`, `database.urlFile`; a local database by default (`database.createLocally`) |
| `SECRET_KEY` | required | At least 32 characters; the server, `opsolete token` and `opsolete rotate-key` refuse to start without it. It encrypts [variables]({{< relref "/docs/features/variables" >}}), sensitive template inputs and VCS access tokens in Postgres and [state files and saved plans]({{< relref "/docs/features/state" >}}) in `STATE_STORE`, signs the [audit log]({{< relref "/docs/config/authentication#audit-log" >}}), and derives the keys of state credentials, signed URLs and `terraform login` codes. Keep it stable. **Back it up outside the deployment** (see [`SECRET_KEY`]({{< relref "/docs/config/secret-key" >}})): without it, encrypted state can't be recovered | `secretKeyFile` |
| `SECRET_KEY_PREVIOUS` | | Only during a [key rotation]({{< relref "/docs/config/secret-key#rotating-it" >}}): the key `SECRET_KEY` replaces. At least 32 characters and different from `SECRET_KEY`. The server still decrypts values encrypted with it (and encrypts only with `SECRET_KEY`), `opsolete rotate-key` re-encrypts them, and the audit log page verifies entries from before the rotation with it | `secretFiles` |
| `STATE_STORE` | required with deployments | Where state files and saved plans live, as a [Go CDK blob URL](https://gocloud.dev/howto/blob/): `azblob://tfstate`, `s3://tfstate?region=eu-west-1` (add `&endpoint=…&use_path_style=true` for S3-compatible stores), `gs://tfstate`, `file:///path` (dev). Credentials come from the platform's usual env vars or workload identity. The server checks at startup that it can reach it | `stateStore` (default: a folder under `/var/lib/opsolete`) |
| `PUBLIC_URL` | `http://opsolete.localhost:8080` | How browsers and Terraform clients reach the app: `http(s)://` and a hostname with a dot, no path. Its host is the registry hostname, and the OIDC callback is `PUBLIC_URL/auth/callback` | `publicUrl` |
| `ADDR` | `:8080` | HTTP listen address | `listenAddress` (`127.0.0.1`), `port` (`8080`) |
| `METRICS_ADDR` | off | Listen address of Prometheus `/metrics` (any path works), e.g. `:9090`. Separate from `ADDR` and unauthenticated: don't expose it publicly. Run metrics come from the database, so every replica reports the same values (use `max`, not `sum`); `opsolete_scheduler_leader` is 1 on the replica running [background work]({{< relref "/docs/operations#background-work" >}}) | |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | | [Login]({{< relref "/docs/config/authentication" >}}) via OpenID Connect, required unless `AUTH_DEV_USER` is set | `oidc.issuer`, `oidc.clientId`, `oidc.clientSecretFile` |
| `ADMIN_GROUPS` | | Comma-separated IdP groups (the `groups` claim; object IDs for Entra ID) that get the admin role. Preferred over `ADMIN_EMAILS` | `adminGroups` |
| `ADMIN_EMAILS` | | Comma-separated emails (the IdP's `email` claim, never `preferred_username`) that get the admin role | `adminEmails` |
| `ORGANIZATION` | `opsolete` | The one organization Terraform's [`cloud` block]({{< relref "/docs/features/terraform-cli" >}}) may name | `organization` |
| `DEPLOYMENTS_ENABLED` | `true` | Templates, workspaces, runs, the runner API and the state backend. `false` makes Opsolete a module registry only | `deployments.enable` |
| `REGISTRY_ENABLED` | `true` | Module registry pages and the Terraform registry protocol. With `false`, startup fails if a template uses a registry module. Both `false` is refused | `registry.enable` |
| `TEMPLATES_DIR` | `catalog` | Local folder of JSON template definitions, for development and tests. In containers, keep the templates in a [Git repository]({{< relref "/docs/features/templates#where-templates-live" >}}) instead; the image and the Nix package ship none | `templatesDir` (default: none) |
| `TEMPLATE_EDITOR` | off | Development: `1` lets the template builder write into `TEMPLATES_DIR` (which must be writable, or startup fails) and turns on module uploads (on in the nix dev shell). A Git template catalog doesn't use it: its editor is set up in **Settings → Templates**, module uploads in **Settings → Organization** | `templateEditor` |
| `STALE_LOCK_TIMEOUT` | `15m` | A rollback's state lock older than this is released as orphaned (the server died mid-rollback). Run locks are released with the run or its lease; CLI and UI locks never expire (unlock or force-unlock them) | `staleLockTimeout` |
| `RUN_LOG_RETENTION_DAYS` | `90` | Run logs are deleted this many days after the run ended (whole days, up to 36500); `0` keeps them forever. The run itself, its status history and plan summary stay; see [Retention]({{< relref "/docs/features/runs#retention" >}}) | `runLogRetentionDays` |
| `AUTH_DEV_USER` | | **Local dev only:** skip login and treat every visitor as this email (set in the nix dev shell) | `devUser` |
| `AUTH_DEV_GROUPS` | | **Local dev only:** comma-separated groups for the dev user, e.g. `grp-a,grp-b` | `devGroups` |

## Runner

`opsolete runner` reads these, then removes every `OPS_*` variable from its environment, so Terraform and module code never see them. See [Runners]({{< relref "/docs/config/runners" >}}).

| Env var | Default | Purpose | NixOS (`runner.…`) |
|---|---|---|---|
| `OPS_SERVER_URL` | required | How the runner reaches Opsolete (in k8s: the Service URL). Registry modules are fetched from here too | `serverUrl` (the local server) |
| `OPS_RUNNER_TOKEN` | required | The runner's own token: `opsolete runner-token <name>` or Settings → Runners ([Runners]({{< relref "/docs/config/runners" >}})) | `tokenFile` (default: `runnerTokenFile`) |
| `OPS_JOB_TIMEOUT` | `1h` | Limit per run phase; then Terraform is interrupted | `jobTimeout` |
| `OPS_JOB_GRACE` | `10m` | After any interrupt (timeout, cancel, lost lease, shutdown), how long Terraform may take to finish in-flight operations and save state before it's killed. In-flight operations such as creating a database can take minutes; a kill can leave real resources missing from state | `jobGrace` |
| `OPS_TERRAFORM` | `terraform` on `PATH` | Terraform binary | `terraformPackage` |
| `OPS_RUNNER_NAME` | hostname | Shown on runs; the pod name in k8s | `name` |
| `OPS_INFRACOST_API_KEY` | off | Turns on [cost estimation]({{< relref "/docs/config/runners#cost-estimation" >}}) (get a key with `infracost auth login`) | `infracost.apiKeyFile` |
| `OPS_INFRACOST` | `infracost` on `PATH` | Infracost binary (the runner image ships 0.10.47); only read with an API key | `infracost.package` |
