---
title: Deployment
weight: 4
---

The app and the runner are the same binary, shipped as two images: `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner`. Settings for both are under [Configuration]({{< relref "configuration" >}}).

## Containers

```sh
just image app && just image runner
export RUNNER_TOKEN=$(openssl rand -hex 32)   # keep it with your other secrets
docker run -e DATABASE_URL=… -e RUNNER_TOKEN -e PUBLIC_URL=https://opsolete.example.com -e OIDC_ISSUER=… -e OIDC_CLIENT_ID=… \
  -e OIDC_CLIENT_SECRET=… -e ADMIN_EMAILS=you@example.com -e STATE_STORE=azblob://tfstate -e AZURE_STORAGE_ACCOUNT=… \
  -p 8080:8080 opsolete-app
docker run -e OPS_SERVER_URL=http://opsolete:8080 -e OPS_RUNNER_TOKEN=$RUNNER_TOKEN opsolete-runner
```

## Kubernetes

The [Helm chart](https://github.com/Opsolete/Helm-charts) does all of the below. It runs as non-root with a read-only root filesystem, and can give the app workload identity for `STATE_STORE` (`workloadIdentity.provider: azure|aws|gcp`). Runners never get one.

```sh
helm repo add opsolete https://opsolete.github.io/Helm-charts
helm install opsolete opsolete/opsolete --set secrets.existingSecret=opsolete \
  --set config.PUBLIC_URL=https://opsolete.example.com --set config.STATE_STORE=azblob://tfstate …
```

- Run two Deployments of the same images: the app, and the runners. The app is stateless: everything it keeps is in Postgres and `STATE_STORE`, and the templates come from a [Git repository]({{< relref "/docs/catalog/templates#where-templates-live" >}}), so it needs no volume.
- Put the runner token in one Secret (`kubectl create secret generic opsolete-runner --from-literal=token=$(openssl rand -hex 32)`, or from sops / External Secrets). Map it to `RUNNER_TOKEN` in the app and to `OPS_RUNNER_TOKEN` in the runners with `secretKeyRef`. Start order doesn't matter: runners retry until the app is up. Cloud credentials come from [variables]({{< relref "/docs/usage/variables" >}}).
- Scale runners with `replicas`, one job each at a time.
- Point `OPS_SERVER_URL` at the app's Service, so registry downloads stay inside the cluster.
- Optionally mount a volume at `TF_PLUGIN_CACHE_DIR` (`/home/runner/.terraform.d/plugin-cache` in the image) to keep providers between restarts.
- Set the runner pods' `terminationGracePeriodSeconds` to at least `OPS_JOB_TIMEOUT` + `OPS_JOB_GRACE` + 5 minutes (see [Shutdown]({{< relref "runners#how-it-works" >}})).
- Scale the app with `replicas` too; background work runs on one of them (below).

## Background work

Background work runs on one app replica at a time, the leader: failing jobs of lost runners (every 15 s), releasing orphaned state locks, queueing rollout batches and drift checks (every 15 s), queueing VCS-triggered runs and syncing the template catalog from Git (every minute), publishing registry module versions from VCS (every 5 minutes), and [retention]({{< relref "/docs/usage/runs#retention" >}}): deleting expired run logs, drift checks and speculative plans, and expired sessions (hourly, at most 1000 runs per statement). The leader holds a PostgreSQL session advisory lock on a connection of its own; the other replicas try to take it every 5 seconds. When the leader stops or loses its database connection, Postgres releases the lock and another replica takes over within about 5 seconds (up to ~30 s if the leader's node vanishes without closing its connection); a job the old leader was running is cancelled. Every replica reloads the template catalog from Postgres every 10 seconds.

Every replica also keeps one more connection of its own, `LISTEN`ing for queued jobs and new log lines (application name `opsolete-notify`), so runner claims and live logs don't poll. If it breaks, the replica logs it and reconnects (backoff up to 30 s); meanwhile claims and log streams look every 5 seconds. Count two connections per replica beside the pool: this one and the leader lock's.

`opsolete_scheduler_leader` (see `METRICS_ADDR`) is 1 on the leader and 0 on the others: alert if `sum` isn't 1. The lock and `LISTEN` need a session: connect through a transaction-pooling proxy (PgBouncer `pool_mode=transaction`) and the leader isn't exclusive, and notifications are lost.

## NixOS

Add this flake as an input and import `opsolete.nixosModules.default` ([nix/module.nix](https://github.com/Opsolete/Opsolete/blob/master/nix/module.nix) lists every option; [Configuration]({{< relref "configuration" >}}) maps them to the env vars). Settings without an option (`METRICS_ADDR`, `SECRET_KEY_PREVIOUS`, cloud credentials) go in `environment`, secret ones in `secretFiles`. Secrets are `*File` options taking a path (e.g. `config.sops.secrets.<name>.path`), passed in through systemd credentials, so they never reach the Nix store. By default it creates a local Postgres database with peer auth.

```nix
services.opsolete = {
  enable = true;
  publicUrl = "https://opsolete.example.com";
  secretKeyFile = config.sops.secrets.opsolete-secret-key.path;
  oidc = { issuer = "https://login.example.com"; clientId = "opsolete"; clientSecretFile = config.sops.secrets.opsolete-oidc.path; };
  runnerTokenFile = config.sops.secrets.opsolete-runner-token.path; # e.g. `openssl rand -hex 32`
  runner.enable = true; # its tokenFile defaults to runnerTokenFile; terraform is unfree
};
```

`opsolete-admin` runs any subcommand with the server's settings: create registry tokens with `sudo opsolete-admin token <name>`, rotate the key with `sudo opsolete-admin rotate-key`. A runner on another host sets `runner.tokenFile` to the same secret. The runner runs in its own PID namespace, so jobs see no other processes of the host. It reads its secret files as root and then drops to its user, so module code (which runs as that user) can't read them. Test: `nix build .#checks.x86_64-linux.nixos` (NixOS VM, needs kvm).
