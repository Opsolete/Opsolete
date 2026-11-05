---
title: Templates
weight: 1
---

Each template is a JSON file that describes one existing Terraform module pinned to an approved `ref`, plus its inputs (`string`, `number`, `bool`, `select`, with optional `required`, `pattern`, `options` and `default`, which must pass the input's own checks). See [`catalog/storage-account.json`](https://github.com/Opsolete/Opsolete/blob/master/catalog/storage-account.json).

Mark a `string` or `number` input `"sensitive": true` for passwords and keys. It is typed in a password field, stored encrypted with `SECRET_KEY`, shown as *(sensitive)*, left out of the audit log, and kept when left empty on reconfigure. It can't have a `default`. A module variable declared `sensitive` in Terraform becomes a sensitive input when the template lists it. Values saved before an input was marked sensitive are encrypted the next time the template is configured.

Mark one input `"key": true` (not `bool` or sensitive) to name the template after its value when it's added to a workspace: a key `vm_name` of `Web-Prod 01` adds it as `module.web_prod_01_c8ad`: a random hex suffix, so the same key can be added twice. Without a key, the template's name is used. The name can't be typed in, and is fixed once added: changing the key's value later doesn't rename it, since that would destroy and recreate its resources.

## Conditional inputs

Add `"show_if": {"input": "<name>", "equals": ["<value>", ...]}` to an input to show it only while an earlier `select` input has one of the values. In the [template builder]({{< relref "template-builder" >}}) this is a field's **Show when … is …** row. Conditions can be chained (a hidden dropdown hides the inputs that depend on it). A hidden input isn't validated or sent to Terraform, so the module's own default applies. Switching a workspace away from a condition drops that input's stored value, sensitive ones included.

A `select` option is either a string or an object `{"label": "...", "value": "..."}`: the form shows the label and Terraform gets the value.

```json
{ "name": "os", "label": "Operating system", "type": "select", "options": [{ "label": "Windows", "value": "windows" }, { "label": "Linux", "value": "linux" }], "default": "linux" },
{ "name": "admin_password", "label": "Admin password", "type": "string", "sensitive": true, "required": true, "show_if": { "input": "os", "equals": ["windows"] } },
{ "name": "ssh_public_key", "label": "SSH public key", "type": "string", "required": true, "show_if": { "input": "os", "equals": ["linux"] } }
```

Things to know:

- **Module variables need a default.** Every variable behind a conditional input needs one (e.g. `default = null`). Without it, Terraform fails with a missing variable whenever that input is hidden.
- **Registry modules drop unknown inputs.** For a template on a [registry]({{< relref "module-registry" >}}) module, the form follows the module's variables, and JSON inputs the module doesn't declare are dropped. If the dropdown a `show_if` refers to (such as `os`) isn't a module variable, every input that depends on it stays hidden. Add it to the module as a variable.
- **A dropdown without a value counts as its first option**, the one the browser shows selected.
- **Without JavaScript, the form doesn't update while you type.** It shows the fields that fit the values it was loaded with. After you change a value and submit, the page reloads with the fields and errors for the new value.

## Where templates live

In production, in a Git repository, so the app keeps no data on its disk:

1. Add a [VCS provider]({{< relref "vcs-providers" >}}) whose token can read the repository.
2. In **Settings → Templates** (admins), choose the connection, repository, branch and folder. The `*.json` files directly in that folder are the catalog.
3. Choose what the [template builder]({{< relref "template-builder" >}}) does on save:
   - **Open a pull request:** a new branch `opsolete/template-<id>-…` with the file and a pull request into the branch. The template changes once it's merged. Works with branch protection.
   - **Commit to the branch:** live right away. The branch must allow direct pushes.
   - **Off:** templates are only edited in Git; a read-only token is enough.

   Saving needs a token with write access: GitHub fine-grained *Contents: read and write* (plus *Pull requests: read and write* for pull requests), Azure DevOps *Code: read & write*. A read-only token fails at the first save, with the provider's error. To give the connection a token with more (or fewer) permissions, use *Change access token* in **Settings → Templates** (the connection's settings): it replaces the token of the whole connection, so its modules and workspaces use it too. If the connection is also open to workspaces, consider a separate connection with its own write token for the catalog.

Every app instance checks the branch every minute (GitHub: a conditional request, free when nothing changed) and on **Sync now**, and loads each new commit. A commit with an invalid template isn't loaded: the page shows why, and the last good commit stays. The last loaded files are kept in Postgres, so an instance starts with the templates even while the provider is down.

A save is based on the loaded commit: if the file changed in Git since (GitHub) or the branch moved (Azure DevOps), the provider refuses it and the catalog reloads, so review and save again; nothing is overwritten. That also makes saving from several replicas safe. A VCS connection the catalog uses can't be deleted; **Disconnect** goes back to `TEMPLATES_DIR`.

Without a repository, templates come from `TEMPLATES_DIR` (the repo's `catalog/` in development).
