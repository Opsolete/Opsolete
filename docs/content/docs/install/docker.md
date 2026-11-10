---
title: Docker
weight: 1
---

The release images are `ghcr.io/opsolete/opsolete-app` and `ghcr.io/opsolete/opsolete-runner` (linux/amd64), tagged with the version (`0.6.0`, `0.6`), `latest` and the commit.

## Try it on one machine

With a throwaway database and state, and no login:

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
- The images are linux/amd64 only. On an arm64 machine such as Apple Silicon, Docker runs them under emulation: the app works, but the runner exits (`harden runner process: PR_SET_CHILD_SUBREAPER`). Run the runner outside Docker there instead (see [Run from source]({{< relref "/docs/install/build" >}})), with `OPS_SERVER_URL=http://localhost:8080`.

For real use, see [Production](#production). Run more runner containers for more jobs at once: each runs one job at a time.

## Production

Use the release images, or build them yourself with `just image app` and `just image runner` as below. A production app needs a fixed `SECRET_KEY`, `PUBLIC_URL`, OIDC and a cloud `STATE_STORE`:

```sh
just image app && just image runner
export RUNNER_TOKEN=$(openssl rand -hex 32)   # keep it with your other secrets
docker run -e DATABASE_URL=… -e RUNNER_TOKEN -e PUBLIC_URL=https://opsolete.example.com -e OIDC_ISSUER=… -e OIDC_CLIENT_ID=… \
  -e OIDC_CLIENT_SECRET=… -e ADMIN_EMAILS=you@example.com -e STATE_STORE=azblob://tfstate -e AZURE_STORAGE_ACCOUNT=… \
  -p 8080:8080 opsolete-app
docker run -e OPS_SERVER_URL=http://opsolete:8080 -e OPS_RUNNER_TOKEN=$RUNNER_TOKEN opsolete-runner
```
