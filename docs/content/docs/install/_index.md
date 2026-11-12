---
title: Install
weight: 2
aliases:
  - /docs/operations/deployment/
---

The app and the runner are the same binary, shipped as two images: `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner`.

Every way to deploy runs the same two parts: the **app** (web UI, module registry, state backend, runner API) and one or more **runners** (they run Terraform and reach the app over HTTP(S), so they need no database access and no open ports). The app needs:

- PostgreSQL 13+.
- `SECRET_KEY`: at least 32 characters, e.g. `openssl rand -base64 32`. It encrypts state, plans and secrets: back it up, losing it loses all state.
- `STATE_STORE`: where state files go, e.g. `azblob://tfstate`, `s3://tfstate?region=eu-west-1` or `gs://tfstate`.
- A login: OIDC (`OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`), and `ADMIN_EMAILS` or `ADMIN_GROUPS` for the first admin.

Each runner has its own token, its `OPS_RUNNER_TOKEN`: create one with `opsolete runner-token <name>` (with the app's settings) or under **Settings → Runners**. See [Runners]({{< relref "/docs/config/runners" >}}).

Every setting is listed in the [option reference]({{< relref "/docs/config/reference" >}}).

{{< cards >}}
  {{< card link="docker" title="Docker" subtitle="The release images, to try Opsolete on one machine or run it in containers." >}}
  {{< card link="kubernetes" title="Kubernetes (Helm)" subtitle="The app and the runners as two Deployments, with the Helm chart." >}}
  {{< card link="nixos" title="NixOS" subtitle="The NixOS module runs the server and a runner as services." >}}
  {{< card link="build" title="Run from source" subtitle="From a checkout, with or without Nix, with examples." >}}
  {{< card link="upgrading" title="Upgrading to 0.6" >}}
  {{< card link="release-notes" title="Release notes" >}}
{{< /cards >}}
