---
title: Kubernetes (Helm)
weight: 2
---

The [Helm chart](https://github.com/Opsolete/Helm-charts) runs the app and the runners as two Deployments. Put the secrets in a Secret with the keys `DATABASE_URL`, `SECRET_KEY`, `RUNNER_TOKEN` (the chart's runners' token: the app registers it at start as runner `runner.name` in `runner.pool`) and `OIDC_CLIENT_SECRET` (or manage it with sops or External Secrets):

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

The chart's [values.yaml](https://github.com/Opsolete/Helm-charts/blob/main/charts/opsolete/values.yaml) lists every option. For example, `runner.replicas` sets how many jobs run at once, and `workloadIdentity` gives the app access to `STATE_STORE` without static keys.

## What the chart sets up

The chart does all of the below, so you need this only to check it or to deploy without it. It runs as non-root with a read-only root filesystem, and can give the app workload identity for `STATE_STORE` (`workloadIdentity.provider: azure|aws|gcp`). Runners never get one.

- Run two Deployments of the same images: the app, and the runners. The app is stateless: everything it keeps is in Postgres and `STATE_STORE`, and the templates come from a [Git repository]({{< relref "/docs/features/templates#where-templates-live" >}}), so it needs no volume.
- Give the runners a token of their own: generate one (`openssl rand -hex 32`) into a Secret (or from sops / External Secrets), map it to `OPS_RUNNER_TOKEN` in the runners with `secretKeyRef`, and register it with the app before it starts: an init container of the app with the app's environment running `/bin/opsolete runner-token -from-env RUNNER_TOKEN -pool default k8s` (the chart's, with `runner.name` and `runner.pool`). Runners that share a Deployment share the token, so they are one runner to the app (one name, one pool); a Deployment per pool keeps production credentials on production runners. Start order doesn't matter: runners retry until the app is up, and stop on a token the app doesn't know. Cloud credentials come from [variables]({{< relref "/docs/features/variables" >}}).
- Scale runners with `replicas`, one job each at a time.
- Block the cloud's metadata endpoint for the runners with a NetworkPolicy, so module code can't take the node's identity, and allow [the job sandbox]({{< relref "/docs/config/runners#the-job-sandbox" >}}) (`seccompProfile: Unconfined` for the runner container, `OPS_SANDBOX=namespaces`) where the nodes allow unprivileged user namespaces.
- Point `OPS_SERVER_URL` at the app's Service, so registry downloads stay inside the cluster.
- Optionally mount a volume at `TF_PLUGIN_CACHE_DIR` (`/home/runner/.terraform.d/plugin-cache` in the image) to keep providers between restarts.
- Set the runner pods' `terminationGracePeriodSeconds` to at least `OPS_JOB_TIMEOUT` + `OPS_JOB_GRACE` + 5 minutes (see [Shutdown]({{< relref "/docs/config/runners#how-it-works" >}})).
- Scale the app with `replicas` too; background work runs on one of them (see [Operations]({{< relref "/docs/operations" >}})).
