---
title: Upgrading to 0.6
weight: 5
aliases:
  - /docs/operations/upgrading/
---

0.6.0 is a rebuild with one new database schema and encryption format. There is no upgrade path from 0.5 or older: install it next to the old one, starting empty.

1. **A new, empty database and state store.** The server refuses a 0.x database ("this database was created by Opsolete 0.x (it has migration …)"), and state files, variables and VCS tokens encrypted by 0.x don't open. Point `DATABASE_URL` at a new database and `STATE_STORE` at a new container or bucket. There is no import: recreate projects, grants, variable sets, VCS connections and workspaces. To keep managing existing resources, bring their state over with Terraform (for example through the [`cloud` block]({{< relref "/docs/features/terraform-cli" >}}) into a local-execution workspace) or import them.
2. **Runners and servers on the same version.** A 0.6 server hands out a claimed job only once the runner acknowledges it; an older runner never does, so its jobs go back to the queue. Upgrade all runners with the server.
3. **Convert the templates.** 0.x template files fail to load: typed defaults, `help` as its own field, no unknown fields. See [Converting 0.x templates]({{< relref "/docs/features/templates#converting-0x-templates" >}}).
4. **Everyone signs in once.** Sessions are stored in the database now; old session cookies don't work. Users log in again, and `terraform login` again for the CLI.
5. **Who creates workspaces.** Only admins and groups with `operate` on a project create workspaces in it, the default project included (UI and `terraform init`). To let everyone create workspaces as before, grant their group `operate` on the default project (*Projects › default › Settings › Team access*). A workspace's *Settings* now need `operate`, and readers get `403` (not `404`) on a state version's values.
6. **Admins:** `ADMIN_EMAILS` matches only the `email` claim, no longer `preferred_username`. Entra ID users without an `email` claim need `ADMIN_GROUPS`.
7. **Rotating `SECRET_KEY`** works differently: restart with `SECRET_KEY_PREVIOUS`, run `opsolete rotate-key` while the servers run, then drop the old key. See [Rotating it]({{< relref "/docs/config/secret-key#rotating-it" >}}).

New settings ([option reference]({{< relref "/docs/config/reference" >}})):

| Env var | Default | |
|---|---|---|
| `ADMIN_GROUPS` | | IdP groups that get the admin role |
| `SECRET_KEY_PREVIOUS` | | The old key during a rotation |
| `RUN_LOG_RETENTION_DAYS` | `90` | Run logs are deleted this long after the run ended; `0` keeps them |
| `OPS_JOB_GRACE` | `10m` | Runner: how long Terraform may take to save state after an interrupt. Set `terminationGracePeriodSeconds` to at least `OPS_JOB_TIMEOUT` + `OPS_JOB_GRACE` + 5 minutes |

From 0.6 on, a new version migrates the database in place when it starts. There is no downgrade: an older version refuses a database a newer one migrated ("this database was migrated by a newer Opsolete …"), so back up the database before upgrading, and to go back, restore that backup.

Drift checks and speculative plans older than 30 days are deleted too ([Retention]({{< relref "/docs/features/runs#retention" >}})). Runners on Linux refuse to start if they can't become a child subreaper ([Runners]({{< relref "/docs/config/runners#how-it-works" >}})).
