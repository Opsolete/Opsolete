---
title: Configuration
weight: 1
---

The server is configured with environment variables. Migrations run on startup. Runner settings are listed under [Runners]({{< relref "runners#settings" >}}).

| Env var         | Default   | Purpose                                |
|-----------------|-----------|----------------------------------------|
| `DATABASE_URL`  | required  | PostgreSQL connection string           |
| `ADDR`          | `:8080`   | HTTP listen address                    |
| `METRICS_ADDR`  | off       | Listen address of Prometheus `/metrics` (any path works), e.g. `:9090`. Separate from `ADDR` and unauthenticated: don't expose it publicly. Run metrics come from the database, so every replica reports the same values (use `max`, not `sum`) |
| `TEMPLATES_DIR` | `catalog` | Local folder of JSON template definitions, for development and tests. In containers, keep the templates in a [Git repository]({{< relref "/docs/catalog/templates#where-templates-live" >}}) instead; the image and the Nix package ship none |
| `DEPLOYMENTS_ENABLED` | `true` | Templates, workspaces, runs, the runner API and the state backend. `false` makes Opsolete a module registry only |
| `REGISTRY_ENABLED` | `true` | Module registry pages and the Terraform registry protocol. With `false`, startup fails if a template uses a registry module |
| `TEMPLATE_EDITOR` | off    | Development: `1` lets the template builder write into `TEMPLATES_DIR` (which must be writable, or startup fails) and turns on module uploads (on in the nix dev shell). A Git template catalog doesn't use it: its editor is set up in **Settings → Templates**, module uploads in **Settings → Organization** |
| `PUBLIC_URL`    | `http://opsolete.localhost:8080` | How browsers and Terraform clients reach the app. Its host is the registry hostname, and the OIDC callback is `PUBLIC_URL/auth/callback` |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | | Login via OpenID Connect, required unless `AUTH_DEV_USER` is set |
| `ADMIN_EMAILS`  |           | Comma-separated emails that get the admin role at login |
| `AUTH_DEV_USER` |           | **Local dev only:** skip login and treat every visitor as this email (set in the nix dev shell) |
| `SECRET_KEY`    | required  | At least 32 characters; the server and `opsolete token` refuse to start without it. It encrypts [variables]({{< relref "/docs/usage/variables" >}}) and VCS access tokens in Postgres, and [state files and saved plans]({{< relref "/docs/usage/state" >}}) in `STATE_STORE`, and signs the [audit log]({{< relref "authentication#audit-log" >}}). Keep it stable. **Back it up outside the deployment** (see [Backing up `SECRET_KEY`]({{< relref "secret-key" >}})): without it, encrypted state can't be recovered |
| `RUNNER_TOKEN`  | required with deployments | At least 32 characters (e.g. `openssl rand -hex 32`). Runners present it as `OPS_RUNNER_TOKEN`. You generate it, so it exists before the first deploy. To rotate it, change it on the server and the runners, then restart both |
| `STATE_STORE`   | required with deployments | Where workspace state files live, as a [Go CDK blob URL](https://gocloud.dev/howto/blob/): `azblob://tfstate`, `s3://tfstate?region=eu-west-1` (add `&endpoint=…&use_path_style=true` for S3-compatible stores), `gs://tfstate`, `file:///path` (dev). Credentials come from the platform's usual env vars or workload identity |
| `STALE_LOCK_TIMEOUT` | `15m` | A rollback's state lock older than this is released as orphaned (the server died mid-rollback). Run locks are released with the run or its lease; CLI and UI locks never expire (use force-unlock) |
| `ORGANIZATION`  | `opsolete` | The one organization Terraform's [`cloud` block]({{< relref "/docs/usage/terraform-cli" >}}) may name |
| `AUTH_DEV_GROUPS` |         | **Local dev only:** comma-separated groups for the dev user, e.g. `grp-a,grp-b` |
