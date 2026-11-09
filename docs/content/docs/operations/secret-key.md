---
title: Backing up SECRET_KEY
weight: 5
---

The state files in `STATE_STORE` can only be read with that key. Back up the bucket and the database as usual, but neither backup restores state without the key.

## Risks

- **Lost key = lost state.** If `SECRET_KEY` is lost, every encrypted state version is unreadable, and so are variables and VCS tokens. Terraform then no longer knows which resources it manages: you'd have to import them all again by hand.
- **Changed key = same as lost.** After just replacing the key, reading old state fails with "sealed with key …, which is neither SECRET_KEY nor SECRET_KEY_PREVIOUS". Never rotate the key by just replacing it: follow the steps below.
- **One key for everything.** The same key protects state files, plans, variables and VCS tokens, so anyone who has it and read access to the bucket or the database can read all of them. Store it as carefully as the cloud credentials themselves.

## Rotating it

Every encrypted value records which key encrypted it (a key id: a hash that doesn't reveal the key), and the server decrypts with `SECRET_KEY` or `SECRET_KEY_PREVIOUS`, so rotating needs restarts, not downtime. Runners aren't involved.

1. Back up the database and `STATE_STORE`. Pick a moment without active runs: run state credentials and signed module URLs are derived from `SECRET_KEY` (not decrypted, so `SECRET_KEY_PREVIOUS` doesn't cover them), so a run under way at the restart, or a plan waiting for confirmation (its saved plan holds the credential), can't read or save its state afterwards. Discard waiting plans and let other runs finish first.
2. Restart every server replica with `SECRET_KEY` set to the new key and `SECRET_KEY_PREVIOUS` to the old one. They read values under either key and write new ones under the new key. Sessions don't depend on the key (they're random tokens in the database), so nobody is signed out, and `terraform login` tokens keep working. Only a sign-in or `terraform login` in progress during the restart has to be started again.
3. Run `opsolete rotate-key` (NixOS: `sudo opsolete-admin rotate-key`) with the same environment, while the servers keep running. It re-encrypts everything not yet under the new key: variables, VCS tokens, sensitive template inputs and every file in `STATE_STORE` (state versions, saved plans), and records the rotation in the audit log. It refuses while a run is active, because a run keeps a copy of encrypted template inputs that it puts back if it fails; wait until runs end (or discard them). If a run starts or a value changes while it works, or it stops halfway, it says so: run it again, values already under the new key are skipped. Values that open with neither key were already unreadable; they are logged and left as they are.
4. When its last log line says so, remove `SECRET_KEY_PREVIOUS` from the servers and restart them. Keep it set only if you want the audit log page to verify entries from before the rotation. Entries written after the rotation must carry the new key, so the old key can't forge them.

Store the new key as described below, and keep the old one until you've checked that state reads.

## Where to keep it

Store the key outside the deployment, apart from the bucket and database backups: in a secret manager (Azure Key Vault, AWS Secrets Manager, GCP Secret Manager, Vault) or a password manager with break-glass access, and inject it from there, e.g. as a Kubernetes secret synced from the vault. Check now and then that the stored copy matches the running one.

If the deployment is lost, restore the database and `STATE_STORE`, set the same `SECRET_KEY`, and all state reads again.
