---
title: VCS providers
weight: 4
---

A VCS connection is an access token for GitHub or Azure DevOps. Registry modules, the [template catalog]({{< relref "templates" >}}) and [workspaces linked to branches]({{< relref "/docs/usage/workspaces#link-a-vcs-repository-branch" >}}) use it; the **VCS providers** page shows what uses each one. Use separate connections where they need different permissions, such as a write token for the template catalog. Requires `SECRET_KEY`.

## Publish modules from GitHub or Azure DevOps

The no-code flow:

1. **Connect** (admins, **Settings → VCS providers** → *Add a VCS provider*, or *Connect to a different VCS* in the new-workspace and publish-module wizards and on **Settings → Templates**, which then continue with the new connection): choose the provider; set it up with a name, API URL and read-only access token; then, under advanced settings, its **workspace scope** (off, all workspaces or [selected ones](#which-workspaces-may-link-a-connection); all workspaces is required when coming from a new workspace). Modules and the template catalog are set up by admins and may use any connection. Change the scope, or replace the token, under the connection's *Settings*: everything that uses the connection switches to the new token.
   - **GitHub:** leave the URL empty for github.com, or use `https://<host>/api/v3` for Enterprise Server; a fine-grained token with *Contents: read*.
   - **Azure DevOps:** `https://dev.azure.com/<org>` and a PAT with *Code (Read)*.
   - **Template catalog:** if templates saved in Opsolete are committed to Git, the token needs write access (see [Templates]({{< relref "templates" >}})).

   The token is checked by listing repos, stored AES-GCM encrypted with `SECRET_KEY`, and never shown again. Changing `SECRET_KEY` makes stored tokens unreadable: delete and re-add the connections.
2. **Publish** (registry page → *Publish module* → *Version control workflow*): pick a connection, a repository and optionally a folder, for a repository with several modules (empty: the repo root). The namespace defaults to the repository name; a repo named `terraform-<system>-<name>` fills in name and system. To change a module's repository or folder, *Unlink* it (registry page or module page; published versions stay) and publish it again.
3. **Versions:** every semver tag (`v1.2.0` or `1.2.0`) becomes an immutable module version, with generated docs, like an upload. The server checks linked repos every 5 minutes and on *Sync now*, so it needs no webhooks or public endpoint. Sync errors (for example a tag without `.tf` files in the module's folder) show on the registry page. Moving a tag doesn't change a version that was already published.
4. **No-code ready:** a template pointing at the module (create one from the module page) makes it *No-code ready* and adds **Provision workspace**. The template is the no-code settings: the pinned version and dropdown options.
5. **Update a workspace:** when the template is moved to a newer version, workspaces on an older one offer the update (see [Update to a new module version]({{< relref "/docs/usage/workspaces#update-to-a-new-module-version" >}})).

## Which workspaces may link a connection

Admins publish modules and load the template catalog through any connection; users link workspaces to its repositories. Users' code runs on the runners, and a run can read its whole repository, so the latter needs limits. Opsolete trusts what's on a linked branch: it can't tell a malicious commit from a good one.

- **Workspace scope** per connection, in its *Settings* on the **VCS providers** page: off (the default for new connections: only admins use it, for modules and templates, and users don't see it), **All workspaces**, or **Selected workspaces and projects**: workspaces picked there in a searchable multi-select, like a variable set's workspaces, and [projects]({{< relref "/docs/usage/projects" >}}) whose every workspace may link. A new workspace can link a connection open to all workspaces, or one that selects the project it's created in (that needs `operate` on the project, so a project's operators decide who links); for a selected workspace, create it first, select it, then link it under its *Version control* settings. A workspace [moved]({{< relref "/docs/usage/projects#manage-projects" >}}) out of a selected project loses the connection. Connections that limited repositories by pattern before this setting existed keep the workspaces already linked through them.
- It's checked when a workspace is linked and again when each run starts, so detaching a workspace stops its runs: they fail with "may not link repositories through VCS connection".
- **Scope the token itself:** the token's reach is the hard limit. On GitHub use a fine-grained token (or one per team) with *Contents: read* on *selected* repositories only, not *All repositories*. On Azure DevOps, a PAT is as broad as its user, so create it for a service account that can only read the projects you want.
- **Protect the linked branches:** anyone who can push to a linked branch runs code with that workspace's credentials. Require pull requests and reviews (GitHub rulesets, Azure DevOps branch policies), restrict who can push, and link production workspaces only to protected branches such as `main`. For tag-based workspaces protect the tags too (GitHub tag rulesets, Azure DevOps tag permissions): anyone who can push a matching tag runs its commit. Only workspaces with **Speculative plans for pull requests** run unreviewed code: every open pull request from the repository (not from forks) is planned with the workspace's variables, so anyone who can push a branch can use them. Turning it on detaches the workspace's variable sets; leave it off for workspaces with production credentials.

## API usage of VCS polling

Opsolete polls instead of receiving webhooks, so it needs no inbound connection from GitHub or Azure DevOps. It keeps polling cheap:

- **One poll per repository branch** per minute, however many workspaces link it: the branch head if a workspace starts runs on new commits, the open pull requests if one plans them, the tags if one is tag-based.
- **GitHub: conditional requests.** Polls send the ETag of the previous answer; when nothing changed GitHub answers `304 Not Modified`, which [doesn't count against the rate limit](https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests). An idle branch costs nothing; a push or pull request update costs one call. Answers are cached in memory per server replica, so each replica's first poll after a start costs one call per branch.
- **Azure DevOps** has no hourly request limit but a [load budget per identity](https://learn.microsoft.com/en-us/azure/devops/integrate/concepts/rate-limits?view=azure-devops); reading refs and pull requests is light.
- **Backing off:** when a provider sends `Retry-After` (both), `x-poll-interval` or an exhausted rate limit (GitHub), polling through that connection pauses until then and the server logs it.

The server talks to the providers' REST APIs (no `git` binary): GitHub's tarball and Azure DevOps' zip download. Archives are repacked with the repo root at the archive root, without symlinks or paths that escape it.
