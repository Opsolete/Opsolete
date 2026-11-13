---
title: First steps
weight: 3
---

From a fresh install to a first managed resource. The steps are the same however you [installed]({{< relref "/docs/install" >}}) Opsolete (Kubernetes, NixOS, Docker or from source): everything below happens in the web UI, as an admin (`ADMIN_EMAILS`), and needs `SECRET_KEY` set (tokens and sensitive variables are stored encrypted) and at least one runner connected (**Settings → Runners**).

The example uses Azure and a storage account behind a private endpoint, from [terraform-modules-examples](https://github.com/Opsolete/terraform-modules-examples/tree/main/storage-account). Any module and cloud work the same way.

## 1. Connect your repositories

**Settings → Providers** (under Version control) **→ Add VCS provider**: pick GitHub or Azure DevOps and paste an access token (for Azure DevOps also the organization URL, `https://dev.azure.com/<org>`). Opsolete checks the token by listing its repositories.

- The module repository needs **read** access (GitHub: *Contents: read*; Azure DevOps: *Code: read*).
- The template repository needs **write** access if the template builder should commit to it (*Contents: read and write*, or *Code: read & write*). Use a separate connection for it if you'd rather keep the module token read-only.

More: [VCS providers]({{< relref "/docs/features/vcs-providers" >}}).

## 2. Publish a module

**Registry → Publish module**, or **Publish module** next to the connection: choose the repository, the module's folder (`storage-account`) and its tag prefix (`storage-account-`), then a namespace, name and provider (`terraform-modules-examples` / `storage-account` / `azurerm`). Every tag `storage-account-v<version>` becomes a registry version, and new tags are picked up by polling.

A module meant for templates configures its own provider and reads credentials from the environment (see [Writing modules for templates]({{< relref "/docs/features/templates#writing-modules-for-templates" >}})). More: [Module registry]({{< relref "/docs/features/module-registry" >}}).

## 3. Connect the template catalog

**Settings → Template catalog**: choose the connection, the repository (`project/repo` for Azure DevOps), the branch and the folder, and what saving a template in the builder does:

- **off**: templates are edited in Git only.
- **commit**: the builder commits to the branch.
- **pr**: the builder opens a pull request; the template is live once it's merged.

Opsolete loads every JSON file in the folder and checks the branch every minute. A commit with an invalid template isn't loaded: the page says which file and why. Until a repository is connected, templates come from `TEMPLATES_DIR`. More: [Where templates live]({{< relref "/docs/features/templates#where-templates-live" >}}).

## 4. Create a template

**Templates → + New template**: pick the registry module and version. The builder generates an input per module variable, with labels taken from the variables' descriptions; tidy the labels, turn fixed choices into dropdowns (region, replication), mark the **key** input (what the workspace's resource is called, here `name`) and keep `existing` as the import switch. **Preview** shows the form as users will see it; **Save** commits it (or opens the pull request).

More: [Template builder]({{< relref "/docs/features/template-builder" >}}), [template format]({{< relref "/docs/features/templates#template-format" >}}).

## 5. Add cloud credentials

**Settings → Variable sets**: name the set (`azure`), tick **global** so every workspace gets it, **Create**, and add environment variables, each marked **sensitive**:

| Key | Value |
|---|---|
| `ARM_TENANT_ID` | the service principal's tenant |
| `ARM_SUBSCRIPTION_ID` | the subscription to deploy in |
| `ARM_CLIENT_ID` | the service principal's app ID |
| `ARM_CLIENT_SECRET` | its secret |

Sensitive values can't be read back and are masked in run logs. Give the service principal only the roles its templates need (e.g. *Contributor* on one resource group or subscription). More: [Variables]({{< relref "/docs/features/variables" >}}).

## 6. Deploy

**Templates → Provision workspace** on the template: name the workspace, fill in the form and submit. A runner plans; review the plan and **Confirm** to apply. Tick **Auto-apply** to skip the confirmation for this workspace.

## 7. Or import what exists

A resource made outside Opsolete (in the portal, say) is taken over through the same template, if its module [can import]({{< relref "/docs/features/templates#writing-modules-that-can-import" >}}):

1. On the template, choose **Import existing**. Fill in what finds the resource: here the account name, resource group, subnet and private endpoint name. The rest can stay at the form's defaults.
2. Opsolete runs a **plan round**: it looks the resources up, imports them in the plan and never applies by itself. The round's page shows, per input, what was **found** next to what it was **planned with**.
3. **Plan again with the found values** until the plan only imports. In the example: round 1 found the region and replication (the defaults would have replaced both resources), round 2 found the tags (it would have removed them), round 3 was `2 to import, 0 to add, 0 to change, 0 to destroy`.
4. **Apply the import**, confirm, and the resources are in the workspace's state, managed by the template. A drift check (**New run → Check for drift**) then shows no changes.

A round that would replace or delete anything can't be applied, and a normal apply on the workspace is refused too while the inputs don't match. More: [Import existing resources]({{< relref "/docs/features/workspaces#import-existing-resources" >}}).

## Next

- [Projects]({{< relref "/docs/features/projects" >}}) and [environments]({{< relref "/docs/features/environments" >}}) to group workspaces and share settings.
- [Teams]({{< relref "/docs/config/authentication#teams-access-per-workspace" >}}) to give users access to workspaces.
- [The Terraform CLI]({{< relref "/docs/features/terraform-cli" >}}) against Opsolete's state and runs.
