---
title: Docker
weight: 1
---

<!-- x-release-please-start-version -->
The release images are `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner` (linux/amd64), tagged with the version (e.g. `0.10.0`; `0.9` follows the latest patch), `latest` and the commit.

## Try it on one machine

With a throwaway database and state, and no login:

```sh
docker network create opsolete
docker run -d --name opsolete-db --network opsolete -e POSTGRES_USER=opsolete -e POSTGRES_PASSWORD=opsolete postgres:17
docker run -d --name opsolete-app --network opsolete -p 8080:8080 \
  -e DATABASE_URL='postgres://opsolete:opsolete@opsolete-db:5432/opsolete?sslmode=disable' \
  -e SECRET_KEY=$(openssl rand -base64 32) \
  -e STATE_STORE='file:///tmp/state?create_dir=1&metadata=skip' \
  -e AUTH_DEV_USER=dev@localhost -e ADMIN_EMAILS=dev@localhost \
  ghcr.io/opsolete/opsolete-app:0.10.0
export OPS_RUNNER_TOKEN=$(docker exec opsolete-app /bin/opsolete runner-token local)  # once the app is up
docker run -d --name opsolete-runner --network opsolete \
  -e OPS_SERVER_URL=http://opsolete-app:8080 -e OPS_RUNNER_TOKEN \
  ghcr.io/opsolete/opsolete-runner:0.10.0
```
<!-- x-release-please-end -->

Then open http://opsolete.localhost:8080. This setup is for trying Opsolete only:

- `AUTH_DEV_USER` logs every visitor in as that user.
- The state in `/tmp` is lost with the container.
- `SECRET_KEY` changes on every run of the command.
- The images are linux/amd64 only. On an arm64 machine such as Apple Silicon, Docker runs them under emulation: the app works, but the runner exits (`harden runner process: PR_SET_CHILD_SUBREAPER`). Run the runner outside Docker there instead (see [Run from source]({{< relref "/docs/install/build" >}})), with `OPS_SERVER_URL=http://localhost:8080`.

The runner starts without [the job sandbox]({{< relref "/docs/config/runners#the-job-sandbox" >}}) and warns: Docker's default seccomp profile forbids it. Add `--security-opt seccomp=unconfined -e OPS_SANDBOX=namespaces` to the runner's `docker run` to turn it on.

For real use, see [Production](#production). Run more runner containers for more jobs at once: each runs one job at a time, and each gets its own token.

## Production

Use the release images, or build them yourself with `just image app` and `just image runner` as below. A production app needs a fixed `SECRET_KEY`, `PUBLIC_URL`, OIDC and a cloud `STATE_STORE`:

```sh
just image app && just image runner
docker run -e DATABASE_URL=… -e PUBLIC_URL=https://opsolete.example.com -e OIDC_ISSUER=… -e OIDC_CLIENT_ID=… \
  -e OIDC_CLIENT_SECRET=… -e ADMIN_EMAILS=you@example.com -e STATE_STORE=azblob://tfstate -e AZURE_STORAGE_ACCOUNT=… \
  -p 8080:8080 opsolete-app
# a runner's token: Settings → Runners, or opsolete runner-token <name> with the app's settings; keep it with your other secrets
docker run -e OPS_SERVER_URL=http://opsolete:8080 -e OPS_RUNNER_TOKEN=… opsolete-runner
```

## Next

[First steps]({{< relref "/docs/first-steps" >}}): connect your repositories, publish a module, create a template, add cloud credentials and deploy or import a first resource.
