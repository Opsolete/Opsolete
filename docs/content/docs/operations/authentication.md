---
title: Login and access
weight: 2
---

All pages require login. Health checks, the registry protocol (`/v1/…`, which uses API tokens and signed URLs), the runner API (`RUNNER_TOKEN`) and the cloud API (`/api/v2/…`, user tokens from `terraform login`) don't.

## Login

- **OIDC:** authorization code flow with PKCE, using `coreos/go-oidc` and `x/oauth2`. It works with Entra ID, Google, Keycloak, Dex and similar. Register `PUBLIC_URL/auth/callback` as the redirect URI. A user is keyed by the ID token's issuer and `sub`; the email (the `email` claim, falling back to `preferred_username` for Entra ID) is an attribute that follows the IdP. Users from before this are bound to the first subject that logs in with their email. A different subject with an email Opsolete already knows is refused, so a reassigned address can't take over the old account or its admin role. An explicit `email_verified: false` is rejected; a missing claim is accepted, since Entra ID never sends it. Changing `OIDC_ISSUER` changes every subject: run `UPDATE users SET subject = NULL` so users rebind by email at their next login.
- **Roles:** emails listed in `ADMIN_EMAILS` are `admin`, everyone else is `user`. The role is recorded at every login, so editing `ADMIN_EMAILS` takes effect at the next login. Sessions are HMAC-signed cookies that last 12 hours. Logging out ends the Opsolete session, not the IdP session.
- **Users** can create workspaces from templates and see and operate their own, plus those granted to one of their groups. **Admins** see and operate every workspace, grant access, use the template builder when it's on and upload modules when that's on (see [Templates]({{< relref "/docs/catalog/templates" >}})).
- Cross-site form posts are rejected (`http.CrossOriginProtection`). If a proxy rewrites the `Host` header, keep it equal to the browser's origin.
- **Local dev:** `AUTH_DEV_USER` skips login and treats every visitor as that email; `AUTH_DEV_GROUPS=grp-a,grp-b` gives the dev user groups.

## Teams: access per workspace

Teams are your IdP's groups. At every login Opsolete records the ID token's `groups` claim for the user, in Postgres because it can be too large for a cookie.

- On a workspace page, an admin grants a group `read` (see the workspace, its runs, logs, outputs and resources) or `operate` (also runs, confirm/discard, destroy, cancel, settings and rename). If a user is in several granted groups, the strongest grant wins.
- Workspaces and their runs only show what the user may see. Workspaces created before login existed have no creator, so only admins see them until a group is granted.
- **Entra ID:** in the app registration, under *Token configuration*, choose *Add groups claim* and select *Security groups* for the ID token. Entra sends group **object IDs**, so grant those. Users in more than 200 groups get an "overage" reference instead of the list, which Opsolete doesn't follow (shortcut: assign groups to the app so the list stays short, or add a Microsoft Graph lookup if that's not enough). Keycloak and Dex need a `groups` mapper or scope.
- Group membership changes take effect at the user's next login. Grants and revokes take effect immediately.

## Audit log

Admins, **Settings → Audit log**, modeled on HCP's: every state-changing request, every sign-in (`GET /auth/callback`, successful or not), and every request refused as unauthenticated or forbidden (401/403), reads included. The page shows the changes; sign-ins and refused requests are on their own tab (`/audit?show=access`).

- Each entry records the principal (`user` for a browser session, `token` for the Terraform CLI, `system` for VCS polling (actor `vcs`) and `opsolete token` (actor `cli`), or `anonymous`) with its id, role and groups; the client address, `X-Forwarded-For` as sent and user agent; method, path and form; and the status and error message.
- Variable values, tokens and OAuth codes are recorded as `(redacted)`.
- The `audit_log` table refuses `UPDATE`, `DELETE` and `TRUNCATE`; each entry carries an HMAC-SHA256 under `SECRET_KEY` over its content and the previous entry's hash, so database access alone can't rewrite or forge entries, and the page re-verifies the chain and names the first broken entry.
- Every entry is also written to stdout as a JSON log line (`"msg":"audit"`, with its hash): ship it with your log collector, which keeps a copy the database owner can't touch and reveals removed newest entries.
