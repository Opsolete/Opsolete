---
title: Deployment
weight: 4
---

The app and the runner are the same binary, shipped as two images: `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner`. Settings are under [Configuration]({{< relref "configuration" >}}) and [Runners]({{< relref "runners#settings" >}}).

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

- Run two Deployments of the same images: the app, and the runners. The app is stateless: everything it keeps is in Postgres and `STATE_STORE`, and the templates come from a [Git repository]({{< relref "/docs/catalog/templates#where-templates-live" >}}), so it needs no volume.
- Put the runner token in one Secret (`kubectl create secret generic opsolete-runner --from-literal=token=$(openssl rand -hex 32)`, or from sops / External Secrets). Map it to `RUNNER_TOKEN` in the app and to `OPS_RUNNER_TOKEN` in the runners with `secretKeyRef`. Start order doesn't matter: runners retry until the app is up. Cloud credentials come from [variables]({{< relref "/docs/usage/variables" >}}).
- Scale runners with `replicas`, one job each at a time.
- Point `OPS_SERVER_URL` at the app's Service, so registry downloads stay inside the cluster.
- Optionally mount a volume at `TF_PLUGIN_CACHE_DIR` to keep providers between restarts.
- Set the runner pods' `terminationGracePeriodSeconds` to cover a job (see [Shutdown]({{< relref "runners#how-it-works" >}})).

## NixOS

Add this flake as an input and import `opsolete.nixosModules.default` ([nix/module.nix](https://github.com/Opsolete/Opsolete/blob/master/nix/module.nix) lists every option). Each env var is an option; secrets are `*File` options taking a path (e.g. `config.sops.secrets.<name>.path`), passed in through systemd credentials, so they never reach the Nix store. By default it creates a local Postgres database with peer auth.

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

Create registry tokens with `sudo opsolete-admin token <name>`. A runner on another host sets `runner.tokenFile` to the same secret. The runner runs as PID 1 of its own PID namespace, so it cleans up after jobs like in a container. Test: `nix build .#checks.x86_64-linux.nixos` (NixOS VM, needs kvm).
