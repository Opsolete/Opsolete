---
title: Backing up SECRET_KEY
weight: 5
---

With `SECRET_KEY` set, the state files in `STATE_STORE` can only be read with that key. Back up the bucket and the database as usual, but neither backup restores state without the key.

## Risks

- **Lost key = lost state.** If `SECRET_KEY` is lost, every encrypted state version is unreadable, and so are variables and VCS tokens. Terraform then no longer knows which resources it manages: you'd have to import them all again by hand.
- **Changed key = same as lost.** After just replacing the key, reading old state fails with "SECRET_KEY changed?". Never rotate the key by just replacing it: use `opsolete rotate-key` (below).
- **One key for everything.** The same key protects state files, plans, variables and VCS tokens, so anyone who has it and read access to the bucket or the database can read all of them. Store it as carefully as the cloud credentials themselves.

## Rotating it

1. Wait until no run is active (or discard them), then stop the server (all replicas). Runners can stay up.
2. Back up the database and `STATE_STORE`.
3. Run `opsolete rotate-key` with the server's usual environment, but `SECRET_KEY` set to the new key and `SECRET_KEY_PREVIOUS` to the current one. It re-seals variables, VCS tokens, sensitive template inputs and every file in `STATE_STORE` (state versions, saved plans) under the new key, and records the rotation in the audit log. It refuses while a run is active. If it stops halfway, run it again: values already under the new key are skipped. Values that open with neither key were already unreadable; they are logged and left as they are.
4. Start the server with the new `SECRET_KEY`. Everyone signs in again, and `terraform login` tokens keep working.
5. Keep `SECRET_KEY_PREVIOUS` set on the server only if you want the audit log page to verify entries from before the rotation: it isn't used for anything else. Entries written after the rotation must carry the new key, so the old key can't forge them.

Store the new key as described below, and keep the old one until you've checked that state reads.

## Where to keep it

Store the key outside the deployment, apart from the bucket and database backups: in a secret manager (Azure Key Vault, AWS Secrets Manager, GCP Secret Manager, Vault) or a password manager with break-glass access, and inject it from there, e.g. as a Kubernetes secret synced from the vault. Check now and then that the stored copy matches the running one.

If the deployment is lost, restore the database and `STATE_STORE`, set the same `SECRET_KEY`, and all state reads again.
