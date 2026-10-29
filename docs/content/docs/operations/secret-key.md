---
title: Backing up SECRET_KEY
weight: 5
---

With `SECRET_KEY` set, the state files in `STATE_STORE` can only be read with that key. Back up the bucket and the database as usual, but neither backup restores state without the key.

## Risks

- **Lost key = lost state.** If `SECRET_KEY` is lost, every encrypted state version is unreadable, and so are variables and VCS tokens. Terraform then no longer knows which resources it manages: you'd have to import them all again by hand.
- **Changed key = same as lost.** There is no key rotation: after a change, reading old state fails with "SECRET_KEY changed?". Never rotate the key by just replacing it.
- **One key for everything.** The same key protects state files, plans, variables and VCS tokens, so anyone who has it and read access to the bucket or the database can read all of them. Store it as carefully as the cloud credentials themselves.

## Where to keep it

Store the key outside the deployment, apart from the bucket and database backups: in a secret manager (Azure Key Vault, AWS Secrets Manager, GCP Secret Manager, Vault) or a password manager with break-glass access, and inject it from there, e.g. as a Kubernetes secret synced from the vault. Check now and then that the stored copy matches the running one.

If the deployment is lost, restore the database and `STATE_STORE`, set the same `SECRET_KEY`, and all state reads again.
