---
title: State
weight: 4
---

Each workspace has exactly one Terraform state. Opsolete is the state backend: runners use Terraform's built-in [`http` backend](https://developer.hashicorp.com/terraform/language/backend/http) against `/api/state/v1/<workspace>`, so they need no storage credentials and the same protocol works on every cloud.

## Storage and versions

- **Storage:** every saved state is a new, immutable **state version**: a file in `STATE_STORE` under `opsolete/<template, or "workspaces">/<workspace>/<serial>-<random>.tfstate`. Postgres keeps only the version list. Writes with an older serial, a different lineage, or the same serial with other content are refused.
- **Outputs** belong to a state version: they are read from the state file when it's saved, sensitive ones stored as `(sensitive)`. The workspace page shows the current version's.
- **Resources:** a workspace page lists the resources (address, type, provider, module) in its **current state version**. The index belongs to a state version. At the end of a run, the runner reads the state back (`terraform show -json`, parsed with HashiCorp's `terraform-json`) and sends names and types only. Attribute values never reach Postgres or the UI.

## Rollback

On a workspace page, *Roll back* next to an older state version saves a copy of it as the new current version (serial one above the current, marked "rollback to vN"), with its outputs and resource index; newer versions are kept. It changes no infrastructure: review the next plan, since resources created or deleted after that version no longer match the state. It takes the state lock and refuses while a run is active.

## Locking

Terraform's `LOCK`/`UNLOCK` calls take a lock in Postgres, so the blob store needs no locking features of its own. A second locker gets `423` with the holder's lock info. When a run ends or its runner disappears, its lock is released; a lock left by a run that no longer holds a lease, or by a rollback older than `STALE_LOCK_TIMEOUT`, is released within 15 seconds and logged.

## Credentials

A run's state password is an HMAC of the workspace and the run. It works only for that workspace (and for reading the states shared with it) and only while that run is planning or applying, so not while a plan waits for confirmation, and never after the run ends. It's per run, not per phase, because Terraform stores the backend config in the saved plan. The server keeps no state passwords.

## Remote state sharing

As in HCP Terraform: under *Settings › General*, a workspace shares its state with all workspaces or with specific ones (default: none). A run of a workspace it is shared with reads it with:

```hcl
data "terraform_remote_state" "x" {
  backend = "http"
  config = {
    address = "<PUBLIC_URL>/api/state/v1/<workspace id>"
  }
}
```

The runner sets `TF_HTTP_USERNAME`/`TF_HTTP_PASSWORD` to the run's own state credentials, so the config needs no secrets. Such a run can only read the state: it can't write or lock it. It gets the whole state file, sensitive values included, just like in HCP.

## Security

State files contain secrets in plain text, and the server can read them. With `SECRET_KEY` set, the server encrypts every state file and saved plan (AES-256-GCM) before it uploads them, so read access to the bucket alone exposes nothing; files written before `SECRET_KEY` was set stay plaintext until the workspace saves state again. Still give the server an identity that can only access the state container, and turn on encryption at rest in the store. Cloud credentials in [variables]({{< relref "variables" >}}) are encrypted with `SECRET_KEY` and only handed to the runner of a claimed job.

{{< callout type="warning" >}}
Without `SECRET_KEY`, encrypted state can't be recovered. See [Backing up `SECRET_KEY`]({{< relref "/docs/operations/secret-key" >}}).
{{< /callout >}}
