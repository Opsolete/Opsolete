# Opsolete
From forms to infrastructure. Automatically.

Pick a template, name a **workspace** and fill in a form generated from JSON. A **Opsolete runner** plans the pinned Terraform module, you review the plan and confirm it, and a runner applies exactly that plan. You watch the output live in the browser, and the Terraform outputs show up when it's done.

**Runs:** every change to a workspace is a run: `plan_queued` → `planning` → `needs_confirmation` → (confirm) `apply_queued` → `applying` → `applied`. A run can also end as `no_changes`, `discarded` (plan not confirmed), `errored` or `canceled`. Each status change lands in the run's history.

- **Plan, then apply.** The plan phase saves a plan file and reports what it will do (`+2 ~1 −0`, plus the resource addresses, never values). Confirming queues the apply phase, which may run on any runner and applies exactly the saved plan. Terraform refuses the apply if the state changed since the plan, so a stale plan can't be applied.
- **Auto-apply** (per workspace, off by default) applies plans without confirmation. **Destroy runs always ask.**
- **One active run per workspace.** A new run waits until the current one is applied, discarded or finished.
- **New run** re-plans with the workspace's inputs, for example to revert drift. Pick **Refresh state only** to run `terraform plan -refresh-only` instead: it shows what changed outside Terraform, and once confirmed (always, even with auto-apply) updates the state to match without changing infrastructure. To **replace** resources, tick them in the workspace's resource list and choose **Plan replacing selected** (`-replace=<address>`; only addresses in the current state are accepted).
- **Drift detection** (per workspace, off by default): once a day the server queues a drift check, a refresh-only plan that is never applied, on workspaces whose state manages resources. **Check for drift** runs one now. Drift shows as a banner on the workspace and a *drifted* tag in the list until the next check or apply. Drift checks aren't listed as runs, but they hold the workspace's one active run while they plan. **Destroy…** plans `terraform plan -destroy`; once confirmed and applied, the workspace is marked destroyed. A new run can recreate it.
- **Cancel run:** a queued run is canceled at once. A running one is asked to stop: the runner interrupts Terraform (SIGINT), which finishes in-flight operations and **still saves state**, and the run ends `canceled`. If a runner doesn't react, **Force cancel** cuts it off immediately; its lease, state password and state lock die with the run, but resources Terraform was creating at that moment may be missing from state.
- **Workspaces don't need a template** (**Workspaces → New workspace**). A blank workspace can stay empty or hold only variables. You can also link it to a **VCS repository branch** (GitHub or Azure DevOps, through a [VCS connection](#vcs-providers-publish-modules-from-github-or-azure-devops), if the connection allows the workspace). Runs then plan the repository root at the branch's latest commit. That commit is pinned for the run, so the apply uses the configuration that was planned. Like HCP Terraform's VCS-driven workflow, the server checks the branch every minute (no webhooks, so Opsolete can stay internal; see [API usage](#api-usage-of-vcs-polling)): **Start a run on every new commit** (on by default for new links) queues an apply run of each new commit that has no run yet, applied with auto-apply or after confirmation. **Speculative plans for pull requests** (opt-in) queue a plan-only run, never applied, for each new commit of an open pull request into the branch; pull requests from forks are skipped. A workspace still has one active run at a time, so VCS runs wait their turn; if several commits land meanwhile, only the newest gets a run. Both settings are on the workspace's **Version control** page, which also shows whether the link works (**Connected to VCS**: the connection allows the workspace and the branch or tags can be read). **Tag-based** triggers replace both, as in HCP Terraform: a new tag in a chosen format (semantic version `1.2.3`, with a prefix `v1.2.3`, with a suffix `1.2.3-rc`, or a custom regular expression) queues an apply run of its commit, manual runs plan the newest matching tag (by the version number in its name; tags without one are never picked), and pull requests get no plans. Runs need configuration: a template, a linked branch, or added templates.
- **Add templates to a workspace**, any number (**Templates → Add a template** on a workspace page, operate access). Each becomes a module block, `module.<name>`, next to the workspace's own configuration (the template it was created from, its branch, or nothing), in the same state; add one template several times under different names. Adding, configuring or removing one starts a plan; removing one plans the destruction of its resources. Removing the last one (the workspace's only configuration) starts a destroy run instead, and the template goes once that destroy is applied. Their outputs show as `<name>__<output>`. Workspace variables and variable sets reach the root module only: env variables (credentials) apply to all of them.
- **Rename** a workspace from its page. State, runs and grants are keyed by its id and stay put.
- **Delete** a workspace from its page once its state manages no resources (destroy them first): runs, variables, grants and state files go with it.
- **Access:** see [Teams](#teams-access-per-workspace). The creator and admins operate a workspace; IdP groups get `read` or `operate` per workspace. Without access, a workspace returns 404, as if it didn't exist.

## Run locally with Nix (recommended)

```sh
nix develop        # or `direnv allow`; gives go, gopls, postgres 17, terraform, just
just               # list tasks
just run           # start project-local postgres (.devdata/, unix socket) + app on :8080
just runner        # in a 2nd terminal: a runner on this machine (state goes to .devdata/state-store)
just reseed        # wipe the dev DB + state files, then `just seed`: example data via the real endpoints
just test          # gofmt check, vet, all tests incl. DB tests
just smoke         # nix build, boot the binary, create a workspace end-to-end
just check         # test + smoke, run before committing
just token <name>  # registry token for Terraform clients
just image app|runner   # container images (DOCKER_HOST=ssh://user@host to build remotely)
just psql | db-reset | down
```

**Examples** (`just seed`, `examples/`): the registry module `examples/greeting/terraform` in versions 1.0.0 and 1.1.0, the `greeting` template (`catalog/greeting.json`), and workspaces of every kind: `greeting-dev` (applied, auto-apply), `greeting-prod` (plan waiting for confirmation, with the `example-azure` variable set), `empty` and `credentials-only` (only variables). The module uses Terraform's built-in `terraform_data`, so a local runner applies it without cloud credentials or provider downloads. All credentials in the examples are fake. `seed` expects an empty database, so use `just reseed` to start over.

`nix build` only sees files tracked by git, so `git add` new files first. After changing dependencies in `go.mod`, update `vendorHash` in `flake.nix` (set it to `nixpkgs.lib.fakeHash`, run `nix build`, and copy the hash it prints).

## Run locally without Nix

Requirements: Go 1.26+, PostgreSQL 13+ (it needs `gen_random_uuid()`).

```sh
docker compose up -d            # or use any local PostgreSQL
export DATABASE_URL=postgres://opsolete:opsolete@localhost:5432/opsolete
export AUTH_DEV_USER=dev@localhost ADMIN_EMAILS=dev@localhost   # local only, see Login
go run ./cmd/opsolete
```

Then open http://localhost:8080. Migrations run on startup.

| Env var         | Default   | Purpose                                |
|-----------------|-----------|----------------------------------------|
| `DATABASE_URL`  | required  | PostgreSQL connection string           |
| `ADDR`          | `:8080`   | HTTP listen address                    |
| `TEMPLATES_DIR` | `catalog` | Directory of JSON template definitions |
| `DEPLOYMENTS_ENABLED` | `true` | Templates, workspaces, runs, the runner API and the state backend. `false` makes Opsolete a module registry only |
| `REGISTRY_ENABLED` | `true` | Module registry pages and the Terraform registry protocol. With `false`, startup fails if a template uses a registry module |
| `TEMPLATE_EDITOR` | off    | `1` enables the template builder and module uploads for admins (on in the nix dev shell) |
| `PUBLIC_URL`    | `http://opsolete.localhost:8080` | How browsers and Terraform clients reach the app. Its host is the registry hostname, and the OIDC callback is `PUBLIC_URL/auth/callback` |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | | Login via OpenID Connect, required unless `AUTH_DEV_USER` is set |
| `ADMIN_EMAILS`  |           | Comma-separated emails that get the admin role at login |
| `AUTH_DEV_USER` |           | **Local dev only:** skip login and treat every visitor as this email (set in the nix dev shell) |
| `SECRET_KEY`    |           | At least 32 characters; encrypts [variables](#variables-and-variable-sets) and VCS access tokens in Postgres, and [state files and saved plans](#workspaces-and-state) in `STATE_STORE`. Set it (and keep it stable) to turn on variables and [VCS providers](#vcs-providers-publish-modules-from-github-or-azure-devops). **Back it up outside the deployment** (see [Backing up `SECRET_KEY`](#backing-up-secret_key)): without it, encrypted state can't be recovered |
| `STATE_STORE`   | required with deployments | Where workspace state files live, as a [Go CDK blob URL](https://gocloud.dev/howto/blob/): `azblob://tfstate`, `s3://tfstate?region=eu-west-1` (add `&endpoint=…&use_path_style=true` for S3-compatible stores), `gs://tfstate`, `file:///path` (dev). Credentials come from the platform's usual env vars or workload identity |
| `ORGANIZATION`  | `opsolete` | The one organization Terraform's [`cloud` block](#terraform-cli-cloud-block-and-terraform-login) may name |

## Login

All pages require login. Health checks, the registry protocol (`/v1/…`, which uses API tokens and signed URLs), the runner API (runner tokens) and the cloud API (`/api/v2/…`, user tokens from `terraform login`) don't.

- **OIDC:** authorization code flow with PKCE, using `coreos/go-oidc` and `x/oauth2`. It works with Entra ID, Google, Keycloak, Dex and similar. Register `PUBLIC_URL/auth/callback` as the redirect URI. The user is identified by the `email` claim, falling back to `preferred_username` (Entra ID). An explicit `email_verified: false` is rejected.
- **Roles:** emails listed in `ADMIN_EMAILS` are `admin`, everyone else is `user`. The role is recorded at every login, so editing `ADMIN_EMAILS` takes effect at the next login. Sessions are HMAC-signed cookies that last 12 hours. Logging out ends the Opsolete session, not the IdP session.
- **Users** can create workspaces from templates and see and operate their own, plus those granted to one of their groups. **Admins** see and operate every workspace, grant access, and with `TEMPLATE_EDITOR=1` use the template builder and upload modules.
- Cross-site form posts are rejected (`http.CrossOriginProtection`). If a proxy rewrites the `Host` header, keep it equal to the browser's origin.

### Teams: access per workspace

Teams are your IdP's groups. At every login Opsolete records the ID token's `groups` claim for the user, in Postgres because it can be too large for a cookie.

- On a workspace page, an admin grants a group `read` (see the workspace, its runs, logs, outputs and resources) or `operate` (also runs, confirm/discard, destroy, cancel, settings and rename). If a user is in several granted groups, the strongest grant wins.
- Workspaces and their runs only show what the user may see. Workspaces created before login existed have no creator, so only admins see them until a group is granted.
- **Entra ID:** in the app registration, under *Token configuration*, choose *Add groups claim* and select *Security groups* for the ID token. Entra sends group **object IDs**, so grant those. Users in more than 200 groups get an "overage" reference instead of the list, which Opsolete doesn't follow (shortcut: assign groups to the app so the list stays short, or add a Microsoft Graph lookup if that's not enough). Keycloak and Dex need a `groups` mapper or scope.
- Group membership changes take effect at the user's next login. Grants and revokes take effect immediately.
- Local dev: `AUTH_DEV_GROUPS=grp-a,grp-b` gives the dev user groups.

Endpoints: `/healthz` (liveness), `/readyz` (checks the DB), `/` (templates), `/templates/{id}` (form, creates a workspace), `/workspaces`, `/workspaces/new` (blank, VCS-linked or from a template), `GET|POST /workspaces/{name}/vcs`, `POST /workspaces/{name}/state-versions/{id}/rollback`, `/v1/vcs-archives/…` (signed, for runners), `/workspaces/{name}` (overview, runs, outputs, resources, state versions), `POST /workspaces/{name}/runs` (`action=apply|destroy|refresh|assess`, `replace=<address>` for apply), `POST …/settings` (name, auto-apply, drift detection), `POST …/delete`, `POST …/access` (admins), `/workspaces/{name}/runs/{id}` (plan, confirm/discard/cancel, live log, history) and its `/logs`, `/auth/login|callback|logout`, `/registry`, `/registry/publish`, `/vcs`, `/vcs/new` (add-provider wizard) and `GET|POST /vcs/{id}/settings` (scope; admins), `GET|POST /workspaces/{name}/configure`, `GET|POST /workspaces/{name}/templates/new?template={id}` and `/workspaces/{name}/templates/{module}` (add or configure an added template), `POST …/templates/{module}/remove`, `/v1/modules/…` (registry protocol), `/api/runner/v1/…` (runner API), `/api/state/v1/{id}` (Terraform state backend), `/api/v2/…` (cloud block API, user tokens), `/oauth/authorization|token` (`terraform login`).

## Terraform CLI: `cloud` block and `terraform login`

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
- **Tokens** from `terraform login` act as you (your role and groups from your last browser login) and expire after 30 days: run `terraform login` again. They also work for the [module registry](#module-registry) on the same host. There's no page to revoke them yet: `DELETE FROM api_tokens WHERE scope = 'user'` revokes them all.
- Like the registry, Terraform only uses HTTPS on a hostname with a dot. For local dev over HTTP, override discovery in a CLI config file (`~/.terraformrc`):

  ```hcl
  host "opsolete.localhost:8080" {
    services = {
      "tfe.v2"   = "http://opsolete.localhost:8080/api/v2/"
      "login.v1" = { client = "terraform-cli", grant_types = ["authz_code"], ports = [10000, 10010],
                     authz = "http://opsolete.localhost:8080/oauth/authorization", token = "http://opsolete.localhost:8080/oauth/token" }
    }
  }
  ```

## Workspaces and state

Each workspace has exactly one Terraform state. Opsolete is the state backend: runners use Terraform's built-in [`http` backend](https://developer.hashicorp.com/terraform/language/backend/http) against `/api/state/v1/<workspace>`, so they need no storage credentials and the same protocol works on every cloud.

- **Storage:** every saved state is a new, immutable **state version**: a file in `STATE_STORE` under `opsolete/<template, or "workspaces">/<workspace>/<serial>-<random>.tfstate`. Postgres keeps only the version list. Writes with an older serial, a different lineage, or the same serial with other content are refused.
- **Outputs** belong to a state version: they are read from the state file when it's saved, sensitive ones stored as `(sensitive)`. The workspace page shows the current version's.
- **Rollback:** on a workspace page, *Roll back* next to an older state version saves a copy of it as the new current version (serial one above the current, marked "rollback to vN"), with its outputs and resource index; newer versions are kept. It changes no infrastructure: review the next plan, since resources created or deleted after that version no longer match the state. It takes the state lock and refuses while a run is active.
- **Locking:** Terraform's `LOCK`/`UNLOCK` calls take a lock in Postgres, so the blob store needs no locking features of its own. A second locker gets `423` with the holder's lock info. When a run ends or its runner disappears, its lock is released.
- **Credentials:** a run's state password is an HMAC of the workspace and the run. It works only for that workspace (and for reading the states shared with it) and only while that run is planning or applying, so not while a plan waits for confirmation, and never after the run ends. It's per run, not per phase, because Terraform stores the backend config in the saved plan. The server keeps no state passwords.
- **Remote state sharing** (as in HCP Terraform): under *Settings › General*, a workspace shares its state with all workspaces or with specific ones (default: none). A run of a workspace it is shared with reads it with `data "terraform_remote_state" "x" { backend = "http", config = { address = "<PUBLIC_URL>/api/state/v1/<workspace id>" } }`. The runner sets `TF_HTTP_USERNAME`/`TF_HTTP_PASSWORD` to the run's own state credentials, so the config needs no secrets. Such a run can only read the state: it can't write or lock it. It gets the whole state file, sensitive values included, just like in HCP.
- **Resources:** a workspace page lists the resources (address, type, provider, module) in its **current state version**. The index belongs to a state version. At the end of a run, the runner reads the state back (`terraform show -json`, parsed with HashiCorp's `terraform-json`) and sends names and types only. Attribute values never reach Postgres or the UI.
- **Security:** state files contain secrets in plain text, and the server can read them. With `SECRET_KEY` set, the server encrypts every state file and saved plan (AES-256-GCM) before it uploads them, so read access to the bucket alone exposes nothing; files written before `SECRET_KEY` was set stay plaintext until the workspace saves state again. Still give the server an identity that can only access the state container, and turn on encryption at rest in the store. Cloud credentials in [variables](#variables-and-variable-sets) are encrypted with `SECRET_KEY` and only handed to the runner of a claimed job.

### Backing up `SECRET_KEY`

With `SECRET_KEY` set, the state files in `STATE_STORE` can only be read with that key. Back up the bucket and the database as usual, but neither backup restores state without the key. Risks:

- **Lost key = lost state.** If `SECRET_KEY` is lost, every encrypted state version is unreadable, and so are variables and VCS tokens. Terraform then no longer knows which resources it manages: you'd have to import them all again by hand.
- **Changed key = same as lost.** There is no key rotation: after a change, reading old state fails with "SECRET_KEY changed?". Never rotate the key by just replacing it.
- **One key for everything.** The same key protects state files, plans, variables and VCS tokens, so anyone who has it and read access to the bucket or the database can read all of them. Store it as carefully as the cloud credentials themselves.

Store the key outside the deployment, apart from the bucket and database backups: in a secret manager (Azure Key Vault, AWS Secrets Manager, GCP Secret Manager, Vault) or a password manager with break-glass access, and inject it from there, e.g. as a Kubernetes secret synced from the vault. Check now and then that the stored copy matches the running one. If the deployment is lost, restore the database and `STATE_STORE`, set the same `SECRET_KEY`, and all state reads again.

## Runners

`opsolete runner` executes runs, one phase (plan or apply) at a time. It's the same binary as the server, packaged as the `runner` image: Alpine, git and Terraform 1.17.0 pinned by checksum, running as a non-root user.

**How it works**

- **Pull, don't push.** Runners long-poll `POST /api/runner/v1/jobs/claim` with a *runner* token. They need no database credentials and accept no inbound connections. Registry tokens can't claim jobs, and runner tokens can't use the registry.
- **Postgres queue.** Queued run phases are the jobs. `FOR UPDATE SKIP LOCKED` lets any number of runners claim without double-running a job.
- **Leases and fencing.** A claim returns a lease id that every log, heartbeat and finish call must send. Log posts double as heartbeats, sent about every 30 s while idle. If a runner disappears for 2 minutes, the server fails the job ("runner stopped responding") and fences the old runner off. A 409 tells a runner it lost the job, so it interrupts Terraform. Jobs are never auto-retried, because a half-applied run needs a human.
- **Isolation.** One job at a time per runner, in a fresh temp directory with its own `HOME`, both removed afterwards, so a job can't leave a `.terraformrc`, `.gitconfig` or provider mirror behind for the next one. Terraform runs via HashiCorp's `terraform-exec`: `init -from-module`, then `plan -out`, `apply <plan>` and `output`. Each run is capped by `OPS_JOB_TIMEOUT`.
- **Jobs can't attack the runner or each other** (Linux). Module code runs as the runner's user, so the runner makes itself non-dumpable: jobs can't read its `/proc/<pid>/environ` or memory, which still hold the runner token. As a container's PID 1 (the image's default), it kills every process a job left behind (e.g. a backgrounded `local-exec`) before claiming the next job, so nothing can watch the next job's credentials. Run it under an init such as `tini` or `docker run --init` and that cleanup is off; the runner warns at startup. The provider plugin cache is shared between jobs; Terraform only uses a cached provider that matches the lock file's checksums.
- **Plans between phases.** The plan phase uploads the plan file and `.terraform.lock.hcl` to the server, which keeps them in `STATE_STORE` (a plan holds variable values, so it's as sensitive as state). The apply phase downloads both and restores the lock file before `init`, so it uses exactly the providers the plan used. Both files are deleted when the run ends.
- **Logs.** Output is shipped about once a second, cut at line ends, and capped at 10 MB per run. The run page follows it live via Server-Sent Events, resuming with `Last-Event-ID` after reconnects. `…/runs/<id>/logs` serves the raw log.
- **Secrets.** Sensitive outputs are shown as `(sensitive)` and never leave the runner. The `output -json` call isn't streamed, and signed module URLs are redacted from logs. The runner reads its `OPS_*` settings and then removes them from its environment, so Terraform, providers and module code never see the runner token.
- **Shutdown.** The first SIGTERM stops claiming and lets the current job finish. A second one interrupts Terraform with SIGINT, so it releases its state lock, and the job ends as `canceled`. Set the pod's `terminationGracePeriodSeconds` to cover a job.

| Env var | Default | Purpose |
|---|---|---|
| `OPS_SERVER_URL` | required | How the runner reaches Opsolete (in k8s: the Service URL). Registry modules are fetched from here too |
| `OPS_RUNNER_TOKEN` | required | `opsolete token <name> runner` |
| `OPS_JOB_TIMEOUT` | `1h` | Hard limit per run phase |
| `OPS_RUNNER_NAME` | hostname | Shown on runs; the pod name in k8s |

Provider credentials come from the workspace's [variables](#variables-and-variable-sets), set as environment variables for Terraform on top of the runner's own environment. Runners are generic: give them no cloud credentials of their own, or every workspace without variables runs with them. Runners need no state storage credentials. Module code runs on the runner with the provider credentials, so scope them to what Opsolete may deploy.

**Containers**

```sh
just image app && just image runner
docker run -e DATABASE_URL=… -e PUBLIC_URL=https://opsolete.example.com -e OIDC_ISSUER=… -e OIDC_CLIENT_ID=… \
  -e OIDC_CLIENT_SECRET=… -e ADMIN_EMAILS=you@example.com -e STATE_STORE=azblob://tfstate -e AZURE_STORAGE_ACCOUNT=… \
  -p 8080:8080 opsolete-app
docker run --rm -e DATABASE_URL=… opsolete-app token runner-1 runner        # prints the runner token
docker run -e OPS_SERVER_URL=http://opsolete:8080 -e OPS_RUNNER_TOKEN=… -e ARM_… opsolete-runner
```

**On Kubernetes:**
- Run two Deployments of the same images: the app, and the runners.
- Pass the runner token as a Secret. Cloud credentials come from [variables](#variables-and-variable-sets).
- Scale runners with `replicas`, one job each at a time.
- Point `OPS_SERVER_URL` at the app's Service, so registry downloads stay inside the cluster.
- Optionally mount a volume at `TF_PLUGIN_CACHE_DIR` to keep providers between restarts.

### Variables and variable sets

A workspace's runs get **variables**: **env** (environment variables, e.g. cloud credentials) and **terraform** (Terraform input variables, passed as strings). Any runner runs any workspace; the server sends the variables with each claimed job.

- **Workspace variables:** the *Variables* section of a workspace page (operate access). A terraform variable can't share a name with one of the template's inputs: change those with *Configure*.
- **Variable sets** (admins, **Variable sets** in the nav): a named group of variables that's **global** (every workspace not linked to a repository) or attached to chosen workspaces. Use one per subscription or service principal, e.g. `azure-prod` with `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, `ARM_CLIENT_ID` and a sensitive `ARM_CLIENT_SECRET`.
- **Repository-linked workspaces get no global sets.** Any user can link a workspace to a branch, and Terraform runs arbitrary code during plan as well as apply (an `external` data source, a provider), so confirming a plan doesn't protect credentials. An admin attaching a set to a VCS-linked workspace vouches for that repository and branch: changing either, switching to or changing tag-based triggers (a tag may point at any commit), or turning on pull request plans (they run unreviewed code), detaches all the workspace's sets. Anyone who can push to the branch can then use the credentials, so protect it (see [Which workspaces may link a connection](#which-workspaces-may-link-a-connection)).
- **Precedence** (same category and key): template inputs > workspace variables > attached sets > global sets. Between sets of the same kind, the first by name wins.
- **Sensitive** variables are write-only: the UI never shows them again, you can only replace or delete them. All values are AES-GCM encrypted with `SECRET_KEY` in Postgres. If `SECRET_KEY` changes, runs that need the old values fail with "can't decrypt": set them again.
- **Reserved env names:** `PATH`, `HOME`, `LD_*`, `DYLD_*`, `GIT_*`, `SSH_*`, `PYTHON*`, `OPS_*`, `TF_*` and a few more are refused, because they could run code on the runner or reconfigure Terraform. Use the terraform category instead of `TF_VAR_*`.
- **Who can read them:** module code and providers see the variables during the run. Anyone with operate access can add env variables, so treat operate access as "may use this workspace's credentials".

Upgrading from runner pools: queued runs of workspaces that had a pool are errored by the migration. Move each pool's credentials into a variable set, attach it to those workspaces, remove the cloud identity from the runner Deployments, and start new runs.

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

After each merge, release-please keeps a **release PR** open. It holds the next version and the `CHANGELOG.md` entries. Merging that PR tags `vX.Y.Z`, creates the GitHub release, and pushes `ghcr.io/opsolete/opsolete-app` and `-runner` tagged `X.Y.Z`, `X.Y` and `latest`. To ship 1.0.0, merge a commit with a `Release-As: 1.0.0` footer. The first release is computed from all of history, so make the commit introducing this MVP a `feat: …`.

`pr-title.yml` rejects PRs whose title isn't a Conventional Commit. With squash merges the title becomes the commit on `master`, so history stays parseable.

**Locking down `master`** (repo settings, one-time):
1. **General → Pull Requests:** allow only *squash merging*, and set its default commit message to *Pull request title*:
   ```sh
   gh api -X PATCH repos/Opsolete/Opsolete -F allow_squash_merge=true -F allow_merge_commit=false \
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

Each `catalog/*.json` file describes one existing Terraform module pinned to an approved `ref`, plus its inputs (`string`, `number`, `bool`, `select`, with optional `required`, `pattern`, `options` and `default`) See `catalog/storage-account.json`.

## Module registry

Opsolete is a private Terraform module registry, built on the standard [module registry protocol](https://developer.hashicorp.com/terraform/internals/module-registry-protocol): `/.well-known/terraform.json`, `GET /v1/modules/:ns/:name/:system/versions` and `.../:version/download`. HashiCorp's `terraform-config-inspect` reads each uploaded module's variables, `terraform-registry-address` validates addresses and `go-version` handles semver.

- **Upload:** on `/registry`, *Publish module* → *Upload workflow*: pick a namespace, name, system and semver version, plus a `.tar.gz` with the module at its root (`tar czf module.tar.gz -C path/to/module .`). Versions are immutable.
- **Use from Terraform:** run `just token my-laptop`, then `export TF_TOKEN_opsolete_example_com=<token>`, then `module "x" { source = "opsolete.example.com/platform/storage/azurerm" version = "1.0.0" }`. Terraform only talks to registries over HTTPS on a hostname with a dot, so put the app behind TLS in production.
- **Docs:** each upload gets generated documentation at `/registry/<ns>/<name>/<system>/<version>`, similar to `terraform-docs`. It includes a usage snippet with the required inputs, Terraform and provider requirements, inputs (type, default, required), outputs, resources, data sources, module calls and the archive's `README.md` shown as plain text. A **Markdown** link (`.../docs.md`) serves HashiCorp's `tfconfig.RenderMarkdown` output to paste into a repo. Versions uploaded before docs existed show a notice; upload a new version to get docs.
- **Downloads:** `download` returns a 10-minute HMAC-signed archive URL, because Terraform doesn't send the token to archive URLs. Runners get a fresh 15-minute signed path with each job. The signing key is kept in Postgres.

### VCS providers: publish modules from GitHub or Azure DevOps

The no-code flow, with `SECRET_KEY` set:

1. **Connect** (admins, **VCS providers** → *Add a VCS provider*, or *Connect to a different VCS* in the new-workspace and publish-module wizards, which then continue with the new connection): choose the provider; set it up with a name, API URL and read-only access token; then, under advanced settings, its scope: **Workspaces** (off, all workspaces or [selected ones](#which-workspaces-may-link-a-connection); all workspaces is required when coming from a new workspace) and **Modules** (whether modules may be published from it; on by default, and linked modules keep syncing when turned off). Change the scope later under the connection's *Settings*. GitHub: leave the URL empty for github.com, or use `https://<host>/api/v3` for Enterprise Server; a fine-grained token with *Contents: read*. Azure DevOps: `https://dev.azure.com/<org>` and a PAT with *Code (Read)*. The token is checked by listing repos, stored AES-GCM encrypted with `SECRET_KEY`, and never shown again. Changing `SECRET_KEY` makes stored tokens unreadable: delete and re-add the connections.
2. **Publish** (registry page → *Publish module* → *Version control workflow*): pick a connection, a repository and a namespace. A repo named `terraform-<system>-<name>` fills in name and system. The module must be at the repo root.
3. **Versions:** every semver tag (`v1.2.0` or `1.2.0`) becomes an immutable module version, with generated docs, like an upload. The server checks linked repos every 5 minutes and on *Sync now*, so it needs no webhooks or public endpoint. Sync errors (for example a tag without `.tf` files at the root) show on the registry page. Moving a tag doesn't change a version that was already published.
4. **No-code ready:** a template pointing at the module (create one from the module page) makes it *No-code ready* and adds **Provision workspace**. The template is the no-code settings: the pinned version and dropdown options.
5. **Update a workspace:** when the template is moved to a newer version, workspaces on an older one show *A new module version is available*. **View update** (or **Configure…** at any time) shows the form filled with the workspace's inputs, plus any new ones. Saving moves the workspace to the template's version and queues a plan, which is confirmed as usual.

#### Which workspaces may link a connection

A connection serves two purposes: admins publish modules from it, and users link workspaces to its repositories. Users' code runs on the runners, and a run can read its whole repository, so the second needs limits. Like HCP Terraform, Opsolete trusts what's on a linked branch: it can't tell a malicious commit from a good one.

- **Workspace scope** per connection, in its *Settings* on the **VCS providers** page: off (the default for new connections: it only publishes modules, and users don't see it), **All workspaces**, or **Selected workspaces**, picked there in a searchable multi-select, like a variable set's workspaces. A new workspace can only link a connection open to all workspaces; for selected ones, create the workspace first, select it, then link it under its *Version control* settings. Connections that limited repositories by pattern before this setting existed keep the workspaces already linked through them.
- It's checked when a workspace is linked and again when each run starts, so detaching a workspace stops its runs: they fail with "may not link repositories through VCS connection".
- **Scope the token itself:** the token's reach is the hard limit. On GitHub use a fine-grained token (or one per team) with *Contents: read* on *selected* repositories only, not *All repositories*. On Azure DevOps, a PAT is as broad as its user, so create it for a service account that can only read the projects you want.
- **Protect the linked branches:** anyone who can push to a linked branch runs code with that workspace's credentials. Require pull requests and reviews (GitHub rulesets, Azure DevOps branch policies), restrict who can push, and link production workspaces only to protected branches such as `main`. For tag-based workspaces protect the tags too (GitHub tag rulesets, Azure DevOps tag permissions): anyone who can push a matching tag runs its commit. Only workspaces with **Speculative plans for pull requests** run unreviewed code: every open pull request from the repository (not from forks) is planned with the workspace's variables, so anyone who can push a branch can use them. Turning it on detaches the workspace's variable sets; leave it off for workspaces with production credentials.

#### API usage of VCS polling

Opsolete polls instead of receiving webhooks, so it needs no inbound connection from GitHub or Azure DevOps. It keeps polling cheap:

- **One poll per repository branch** per minute, however many workspaces link it: the branch head if a workspace starts runs on new commits, the open pull requests if one plans them, the tags if one is tag-based.
- **GitHub: conditional requests.** Polls send the ETag of the previous answer; when nothing changed GitHub answers `304 Not Modified`, which [doesn't count against the rate limit](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests). An idle branch costs nothing; a push or pull request update costs one call. Answers are cached in memory per server replica, so each replica's first poll after a start costs one call per branch.
- **Azure DevOps** has no hourly request limit but a [load budget per identity](https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits?view=azure-devops); reading refs and pull requests is light.
- **Backing off:** when a provider sends `Retry-After` (both), `x-poll-interval` or an exhausted rate limit (GitHub), polling through that connection pauses until then and the server logs it.

The server talks to the providers' REST APIs (no `git` binary): GitHub's tarball and Azure DevOps' zip download. Archives are repacked with the repo root at the archive root, without symlinks or paths that escape it.

## Template builder

With `TEMPLATE_EDITOR=1`, open **+ New template** (`/templates/new`) or **edit** on any template. Choose **Start from a registry module** and the module source, version and one form field per variable are filled in for you. Complex variable types (list, map, object) are flagged, not generated. You can also type a `git::` source and ref by hand, then add one row per form field (variable name, label, type, required, pattern, dropdown options, default). Each new template gets a random UUID as its id. Saving runs the same validation as startup, checks that a referenced registry version exists, writes `catalog/<id>.json` and shows the generated deploy form right away. Review and commit the file like any other change. Only admins can use the builder, because a template decides which module the runners execute.
