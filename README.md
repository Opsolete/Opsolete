# Form2Stack
From forms to infrastructure. Automatically.

Pick a template, name a **workspace** and fill in a form generated from JSON. A **Form2Stack runner** plans the pinned Terraform module, you review the plan and confirm it, and a runner applies exactly that plan. You watch the output live in the browser, and the Terraform outputs show up when it's done. It works like HCP Terraform's workspaces and runs.

**Runs:** every change to a workspace is a run: `plan_queued` → `planning` → `needs_confirmation` → (confirm) `apply_queued` → `applying` → `applied`. A run can also end as `no_changes`, `discarded` (plan not confirmed), `errored` or `canceled`. Each status change lands in the run's history.

- **Plan, then apply.** The plan phase saves a plan file and reports what it will do (`+2 ~1 −0`, plus the resource addresses, never values). Confirming queues the apply phase, which may run on any runner and applies exactly the saved plan. Terraform refuses the apply if the state changed since the plan, so a stale plan can't be applied.
- **Auto-apply** (per workspace, off by default) applies plans without confirmation. **Destroy runs always ask.**
- **One active run per workspace.** A new run waits until the current one is applied, discarded or finished.
- **New run** re-plans with the workspace's inputs, for example to fix drift. **Destroy…** plans `terraform plan -destroy`; once confirmed and applied, the workspace is marked destroyed. A new run can recreate it.
- Only the user who created a workspace, or an admin, can start runs, confirm or discard plans and change its settings. Everyone logged in can see workspaces, runs and logs.

## Run locally with Nix (recommended)

```sh
nix develop        # or `direnv allow`; gives go, gopls, postgres 17, terraform, just
just               # list tasks
just run           # start project-local postgres (.devdata/, unix socket) + app on :8080
just runner        # in a 2nd terminal: a runner on this machine (state goes to .devdata/state-store)
just test          # gofmt check, vet, all tests incl. DB tests
just smoke         # nix build, boot the binary, create a workspace end-to-end
just check         # test + smoke, run before committing
just token <name>  # registry token for Terraform clients
just image app|runner   # container images (DOCKER_HOST=ssh://user@host to build remotely)
just psql | db-reset | down
```

`nix build` only sees files tracked by git, so `git add` new files first. After changing dependencies in `go.mod`, update `vendorHash` in `flake.nix` (set it to `nixpkgs.lib.fakeHash`, run `nix build`, and copy the hash it prints).

## Run locally without Nix

Requirements: Go 1.26+, PostgreSQL 13+ (it needs `gen_random_uuid()`).

```sh
docker compose up -d            # or use any local PostgreSQL
export DATABASE_URL=postgres://form2stack:form2stack@localhost:5432/form2stack
export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost   # local only, see Login
go run ./cmd/form2stack
```

Then open http://localhost:8080. Migrations run on startup.

| Env var         | Default   | Purpose                                |
|-----------------|-----------|----------------------------------------|
| `DATABASE_URL`  | required  | PostgreSQL connection string           |
| `ADDR`          | `:8080`   | HTTP listen address                    |
| `TEMPLATES_DIR` | `catalog` | Directory of JSON template definitions |
| `DEPLOYMENTS_ENABLED` | `true` | Templates, workspaces, runs, the runner API and the state backend. `false` makes Form2Stack a module registry only |
| `REGISTRY_ENABLED` | `true` | Module registry pages and the Terraform registry protocol. With `false`, startup fails if a template uses a registry module |
| `TEMPLATE_EDITOR` | off    | `1` enables the template builder and module uploads for admins (on in the nix dev shell) |
| `PUBLIC_URL`    | `http://form2stack.localhost:8080` | How browsers and Terraform clients reach the app. Its host is the registry hostname, and the OIDC callback is `PUBLIC_URL/auth/callback` |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | | Login via OpenID Connect, required unless `AUTH_DEV_USER` is set |
| `ADMIN_EMAILS`  |           | Comma-separated emails that get the admin role at login |
| `AUTH_DEV_USER` |           | **Local dev only:** skip login and treat every visitor as this email (set in the nix dev shell) |
| `STATE_STORE`   | required with deployments | Where workspace state files live, as a [Go CDK blob URL](https://gocloud.dev/howto/blob/): `azblob://tfstate`, `s3://tfstate?region=eu-west-1` (add `&endpoint=…&use_path_style=true` for S3-compatible stores), `gs://tfstate`, `file:///path` (dev). Credentials come from the platform's usual env vars or workload identity |

## Login

All pages require login. Health checks, the registry protocol (`/v1/…`, which uses API tokens and signed URLs) and the runner API (runner tokens) don't.

- **OIDC:** authorization code flow with PKCE, using `coreos/go-oidc` and `x/oauth2`. It works with Entra ID, Google, Keycloak, Dex and similar. Register `PUBLIC_URL/auth/callback` as the redirect URI. The user is identified by the `email` claim, falling back to `preferred_username` (Entra ID). An explicit `email_verified: false` is rejected.
- **Roles:** emails listed in `ADMIN_EMAILS` are `admin`, everyone else is `user`. The role is recorded at every login, so editing `ADMIN_EMAILS` takes effect at the next login. Sessions are HMAC-signed cookies that last 12 hours. Logging out ends the Form2Stack session, not the IdP session.
- **Users** can create workspaces from templates, see all workspaces and runs, and operate their own workspaces (runs, confirm/discard, destroy, settings). **Admins** can operate any workspace and, with `TEMPLATE_EDITOR=1`, use the template builder and upload modules.
- Cross-site form posts are rejected (`http.CrossOriginProtection`). If a proxy rewrites the `Host` header, keep it equal to the browser's origin.

Endpoints: `/healthz` (liveness), `/readyz` (checks the DB), `/` (templates), `/templates/{id}` (form, creates a workspace), `/workspaces`, `/workspaces/{name}` (overview, runs, outputs, resources, state versions), `POST /workspaces/{name}/runs` (`action=apply|destroy`), `/workspaces/{name}/runs/{id}` (plan, confirm/discard, live log, history) and its `/logs`, `/auth/login|callback|logout`, `/resources`, `/registry`, `/v1/modules/…` (registry protocol), `/api/runner/v1/…` (runner API), `/api/state/v1/{id}` (Terraform state backend).

## Workspaces and state

Each workspace has exactly one Terraform state. Form2Stack is the state backend, the way HCP Terraform is: runners use Terraform's built-in [`http` backend](https://developer.hashicorp.com/terraform/language/backend/http) against `/api/state/v1/<workspace>`, so they need no storage credentials and the same protocol works on every cloud.

- **Storage:** every saved state is a new, immutable **state version**: a file in `STATE_STORE` under `form2stack/<template>/<workspace>/<serial>-<random>.tfstate`. Postgres keeps only the version list. Writes with an older serial or a different lineage are refused.
- **Locking:** Terraform's `LOCK`/`UNLOCK` calls take a lock in Postgres, so the blob store needs no locking features of its own. A second locker gets `423` with the holder's lock info. When a run ends or its runner disappears, its lock is released.
- **Credentials:** a run's state password is an HMAC of the workspace and the run. It works only for that workspace and only while that run is planning or applying, so not while a plan waits for confirmation, and never after the run ends. It's per run, not per phase, because Terraform stores the backend config in the saved plan. The server keeps no state passwords.
- **Resources page:** `/resources` lists the resources (address, type, provider, module) in every workspace's **current state version**, with a filter. The index belongs to a state version, like HCP Terraform's. At the end of a run, the runner reads the state back (`terraform show -json`, parsed with HashiCorp's `terraform-json`) and sends names and types only. Attribute values never reach Postgres or the UI.
- **Security:** state files contain secrets in plain text, and the server can read them. Give the server an identity that can only access the state container, and turn on encryption at rest in the store. The server never holds cloud credentials to change infrastructure.

## Runners

`form2stack runner` executes runs, one phase (plan or apply) at a time. It's the same binary as the server, packaged as the `runner` image: Alpine, git and Terraform 1.17.0 pinned by checksum, running as a non-root user.

**How it works**

- **Pull, don't push.** Runners long-poll `POST /api/runner/v1/jobs/claim` with a *runner* token. They need no database credentials and accept no inbound connections. Registry tokens can't claim jobs, and runner tokens can't use the registry.
- **Postgres queue.** Queued run phases are the jobs. `FOR UPDATE SKIP LOCKED` lets any number of runners claim without double-running a job.
- **Leases and fencing.** A claim returns a lease id that every log, heartbeat and finish call must send. Log posts double as heartbeats, sent about every 30 s while idle. If a runner disappears for 2 minutes, the server fails the job ("runner stopped responding") and fences the old runner off. A 409 tells a runner it lost the job, so it interrupts Terraform. Jobs are never auto-retried, because a half-applied run needs a human.
- **Isolation.** One job at a time per runner, in a fresh temp directory that's removed afterwards. Terraform runs via HashiCorp's `terraform-exec`: `init -from-module`, then `plan -out`, `apply <plan>` and `output`. Each run is capped by `F2S_JOB_TIMEOUT`.
- **Plans between phases.** The plan phase uploads the plan file and `.terraform.lock.hcl` to the server, which keeps them in `STATE_STORE` (a plan holds variable values, so it's as sensitive as state). The apply phase downloads both and restores the lock file before `init`, so it uses exactly the providers the plan used. Both files are deleted when the run ends.
- **Logs.** Output is shipped about once a second, cut at line ends, and capped at 10 MB per run. The run page follows it live via Server-Sent Events, resuming with `Last-Event-ID` after reconnects. `…/runs/<id>/logs` serves the raw log.
- **Secrets.** Sensitive outputs are shown as `(sensitive)` and never leave the runner. The `output -json` call isn't streamed, and signed module URLs are redacted from logs. The runner reads its `F2S_*` settings and then removes them from its environment, so Terraform, providers and module code never see the runner token.
- **Shutdown.** The first SIGTERM stops claiming and lets the current job finish. A second one interrupts Terraform with SIGINT, so it releases its state lock, and the job ends as `canceled`. Set the pod's `terminationGracePeriodSeconds` to cover a job.

| Env var | Default | Purpose |
|---|---|---|
| `F2S_SERVER_URL` | required | How the runner reaches Form2Stack (in k8s: the Service URL). Registry modules are fetched from here too |
| `F2S_RUNNER_TOKEN` | required | `form2stack token <name> runner` |
| `F2S_JOB_TIMEOUT` | `1h` | Hard limit per run phase |
| `F2S_RUNNER_NAME` | hostname | Shown on runs; the pod name in k8s |

Provider credentials go to the runner as the usual env vars, for example `ARM_*` for azurerm (on AKS: `ARM_USE_AKS_WORKLOAD_IDENTITY=true`). Runners need no state storage credentials. Module code runs on the runner with the provider credentials, so scope them to what Form2Stack may deploy.

**Containers**

```sh
just image app && just image runner
docker run -e DATABASE_URL=… -e PUBLIC_URL=https://form2stack.example.com -e OIDC_ISSUER=… -e OIDC_CLIENT_ID=… \
  -e OIDC_CLIENT_SECRET=… -e ADMIN_EMAILS=you@example.com -e STATE_STORE=azblob://tfstate -e AZURE_STORAGE_ACCOUNT=… \
  -p 8080:8080 form2stack-app
docker run --rm -e DATABASE_URL=… form2stack-app token runner-1 runner        # prints the runner token
docker run -e F2S_SERVER_URL=http://form2stack:8080 -e F2S_RUNNER_TOKEN=… -e ARM_… form2stack-runner
```

**On Kubernetes:**
- Run two Deployments of the same images.
- Pass the runner token and cloud credentials as Secrets (or workload identity).
- Scale runners with `replicas`, one job each at a time.
- Point `F2S_SERVER_URL` at the app's Service, so registry downloads stay inside the cluster.
- Optionally mount a volume at `TF_PLUGIN_CACHE_DIR` to keep providers between restarts.

## CI and releases

`.github/workflows/ci.yml` runs on every PR and every push to `master`:
- **`test`:** gofmt, vet, and all tests against a Postgres service, with Terraform 1.17.0 for the runner tests.
- **`nix`:** `nix build`, which also catches a stale `vendorHash`.
- **`images`:** builds the `app` and `runner` images. They're pushed **only when a release is created**.

**Versions come from commit messages** ([Conventional Commits](https://www.conventionalcommits.org) → SemVer, via [release-please](https://github.com/googleapis/release-please)):

| Commit (= PR title, squash-merged) | Next version from 0.4.2 | Once ≥ 1.0.0 |
|---|---|---|
| `fix: …`, `perf: …` | 0.4.3 | patch |
| `feat: …` | 0.5.0 | minor |
| `feat!: …` or a `BREAKING CHANGE:` footer | 0.5.0 (pre-1.0, breaking bumps minor) | major |
| `docs:`, `chore:`, `ci:`, `test:`, `refactor:`, … | no release | no release |

After each merge, release-please keeps a **release PR** open. It holds the next version and the `CHANGELOG.md` entries. Merging that PR tags `vX.Y.Z`, creates the GitHub release, and pushes `ghcr.io/rudesome/form2stack-app` and `-runner` tagged `X.Y.Z`, `X.Y` and `latest`. To ship 1.0.0, merge a commit with a `Release-As: 1.0.0` footer. The first release is computed from all of history, so make the commit introducing this MVP a `feat: …`.

`pr-title.yml` rejects PRs whose title isn't a Conventional Commit. With squash merges the title becomes the commit on `master`, so history stays parseable.

**Locking down `master`** (repo settings, one-time):
1. **General → Pull Requests:** allow only *squash merging*, and set its default commit message to *Pull request title*:
   ```sh
   gh api -X PATCH repos/rudesome/Form2Stack -F allow_squash_merge=true -F allow_merge_commit=false \
     -F allow_rebase_merge=false -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY -F delete_branch_on_merge=true
   ```
2. **Rules → Rulesets → New branch ruleset** for `master`: require a pull request, require the status checks `test`, `nix`, `images (app)`, `images (runner)` and `conventional-commit`, block force pushes, and restrict deletions.
3. **Release PR checks:** PRs opened with the default `GITHUB_TOKEN` don't trigger workflows, so the release PR would never get its required checks. Add a fine-grained PAT, or a GitHub App token with *contents* and *pull requests* write access, as the secret `RELEASE_PLEASE_TOKEN`.
4. **Images:** GHCR packages start out private. Make them public, or give your cluster a pull secret.

## Tests

```sh
go test ./...
TEST_DATABASE_URL=$DATABASE_URL go test ./internal/storage   # database tests
```

## Templates

Each `catalog/*.json` file describes one existing Terraform module pinned to an approved `ref`, plus its inputs (`string`, `number`, `bool`, `select`, with optional `required`, `pattern`, `options` and `default`). See `catalog/storage-account.json`.

## Module registry

Form2Stack is a private Terraform module registry, built on the standard [module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol): `/.well-known/terraform.json`, `GET /v1/modules/:ns/:name/:system/versions` and `.../:version/download`. HashiCorp's `terraform-config-inspect` reads each uploaded module's variables, `terraform-registry-address` validates addresses and `go-version` handles semver.

- **Upload:** on `/registry`, pick a namespace, name, system and semver version, plus a `.tar.gz` with the module at its root (`tar czf module.tar.gz -C path/to/module .`). Versions are immutable.
- **Use from Terraform:** run `just token my-laptop`, then `export TF_TOKEN_form2stack_example_com=<token>`, then `module "x" { source = "form2stack.example.com/platform/storage/azurerm" version = "1.0.0" }`. Terraform only talks to registries over HTTPS on a hostname with a dot, so put the app behind TLS in production.
- **Docs:** each upload gets generated documentation at `/registry/<ns>/<name>/<system>/<version>`, similar to `terraform-docs`. It includes a usage snippet with the required inputs, Terraform and provider requirements, inputs (type, default, required), outputs, resources, data sources, module calls and the archive's `README.md` shown as plain text. A **Markdown** link (`.../docs.md`) serves HashiCorp's `tfconfig.RenderMarkdown` output to paste into a repo. Versions uploaded before docs existed show a notice; upload a new version to get docs.
- **Downloads:** `download` returns a 10-minute HMAC-signed archive URL, because Terraform doesn't send the token to archive URLs. Runners get a fresh 15-minute signed path with each job. The signing key is kept in Postgres.

## Template builder

With `TEMPLATE_EDITOR=1`, open **+ New template** (`/templates/new`) or **edit** on any template. Choose **Start from a registry module** and the module source, version and one form field per variable are filled in for you. Complex variable types (list, map, object) are flagged, not generated. You can also type a `git::` source and ref by hand, then add one row per form field (variable name, label, type, required, pattern, dropdown options, default). Each new template gets a random UUID as its id. Saving runs the same validation as startup, checks that a referenced registry version exists, writes `catalog/<id>.json` and shows the generated deploy form right away. Review and commit the file like any other change. Only admins can use the builder, because a template decides which module the runners execute.
