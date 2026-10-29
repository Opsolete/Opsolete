---
title: Templates
weight: 1
---

Each template is a JSON file that describes one existing Terraform module pinned to an approved `ref`, plus its inputs (`string`, `number`, `bool`, `select`, with optional `required`, `pattern`, `options` and `default`, which must pass the input's own checks). See [`catalog/storage-account.json`](https://github.com/Opsolete/Opsolete/blob/master/catalog/storage-account.json).

## Where templates live

In production, in a Git repository, so the app keeps no data on its disk:

1. Add a [VCS provider]({{< relref "vcs-providers" >}}) whose token can read the repository.
2. In **Settings → Templates** (admins), choose the connection, repository, branch and folder. The `*.json` files directly in that folder are the catalog.
3. Choose what the [template builder]({{< relref "template-builder" >}}) does on save:
   - **Open a pull request:** a new branch `opsolete/template-<id>-…` with the file and a pull request into the branch. The template changes once it's merged. Works with branch protection.
   - **Commit to the branch:** live right away. The branch must allow direct pushes.
   - **Off:** templates are only edited in Git; a read-only token is enough.

   Saving needs a token with write access: GitHub fine-grained *Contents: read and write* (plus *Pull requests: read and write* for pull requests), Azure DevOps *Code: read & write*. A read-only token fails at the first save, with the provider's error.

Every app instance checks the branch every minute (GitHub: a conditional request, free when nothing changed) and on **Sync now**, and loads each new commit. A commit with an invalid template isn't loaded: the page shows why, and the last good commit stays. The last loaded files are kept in Postgres, so an instance starts with the templates even while the provider is down.

A save is based on the loaded commit: if the file changed in Git since (GitHub) or the branch moved (Azure DevOps), the provider refuses it and the catalog reloads, so review and save again; nothing is overwritten. That also makes saving from several replicas safe. A VCS connection the catalog uses can't be deleted; **Disconnect** goes back to `TEMPLATES_DIR`.

Without a repository, templates come from `TEMPLATES_DIR` (the repo's `catalog/` in development).
