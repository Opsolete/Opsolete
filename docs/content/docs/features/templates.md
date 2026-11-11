---
title: Templates
weight: 8
aliases:
  - /docs/catalog/templates/
---

Each template is a JSON file that describes one existing Terraform module pinned to an approved `ref`, plus the inputs its form asks. This is the template file format 1.0:

```json
{
  "$schema": "https://opsolete.github.io/Opsolete/schema/template-1.json",
  "id": "storage-account",
  "name": "Azure Storage Account",
  "description": "A general-purpose v2 storage account with optional blob versioning.",
  "module": {
    "source": "git::https://dev.azure.com/your-org/infra/_git/terraform-modules//storage-account",
    "ref": "v1.0.0"
  },
  "inputs": [
    { "name": "name", "label": "Account name", "help": "3-24 lowercase letters or digits, unique in Azure.", "type": "string", "required": true, "pattern": "^[a-z0-9]{3,24}$", "key": true },
    { "name": "location", "label": "Region", "type": "select", "options": ["westeurope", { "value": "northeurope", "label": "North Europe" }], "default": "westeurope" },
    { "name": "retention_days", "label": "Soft-delete retention (days)", "type": "number", "default": 7 },
    { "name": "enable_versioning", "label": "Enable blob versioning", "type": "bool", "default": true }
  ]
}
```

See also [`catalog/storage-account.json`](https://github.com/Opsolete/Opsolete/blob/master/catalog/storage-account.json).

## Template format

**Template** (the file):

| Field | Type | | |
|---|---|---|---|
| `$schema` | string | optional | The JSON Schema URL; allowed and ignored by Opsolete. |
| `id` | string | required | Lowercase letters, digits and dashes, at most 63 characters, not `new`. Unique in the catalog. The builder gives new templates a UUID. |
| `name` | string | required | The title in the catalog. |
| `description` | string | optional | One sentence, shown on the catalog card. |
| `module` | object | required | `source`: a `git::` URL or a [registry]({{< relref "/docs/features/module-registry" >}}) module address; `ref`: the approved Git ref or registry version. Both required. |
| `inputs` | array | optional | The form's fields, in order. |

**Input:**

| Field | Type | | |
|---|---|---|---|
| `name` | string | required | The Terraform variable it sets: `^[a-z_][a-z0-9_]*$`, not `source`, `version`, `providers`, `count`, `for_each`, `lifecycle`, `depends_on` or `locals`. Unique in the template. |
| `type` | string | required | `string`, `number`, `bool` (a checkbox), `select` (a dropdown), or for Terraform's complex types `list` (rows of fields), `string_list` (rows of text), `map` (key / value rows) or `object` (a group of fields); see [lists, maps and objects](#lists-maps-and-objects). |
| `label` | string | optional | The question the form asks. Empty: the form shows the name. |
| `help` | string | optional | Shown under the field (and linked to it for screen readers), and the error message when a value doesn't match `pattern`, so say there what fits. Without help, the error shows the pattern. |
| `required` | boolean | optional | The value can't be empty. Dropdowns and checkboxes always have a value. A hidden input is never required. |
| `pattern` | string | optional | A Go (RE2) regular expression the whole value must match. The browser checks it too, as a JavaScript one. |
| `options` | array | `select` only, required there | One entry per choice: a string (shorthand for `{"value": "<string>"}`) or `{"value": "...", "label": "..."}`: the form shows `label`, Terraform gets `value`. `value` is required and not empty. |
| `default` | typed | optional | Filled in when the form opens. Typed by `type`: `string` and `select` a JSON string (a select's must be one of its option values), `number` a JSON number (`7`, `0.5`), `bool` a JSON boolean (`true`, `false`), `list` a JSON array of objects (the rows), `string_list` an array of strings, `map` an object of strings, `object` an object. It must pass the input's own checks (`pattern`). |
| `sensitive` | boolean | optional | `string` and `number` only, no `default`; see below. |
| `show_if` | object | optional | `{"input": "<name>", "equals": [...]}`; see [conditional inputs](#conditional-inputs). |
| `key` | boolean | optional | At most one input, not `bool` or sensitive; see below. |
| `per_environment` | boolean | optional | Not with `sensitive` or `key`. Each project may set the value for all its workspaces; see below. |
| `fields` | array | `list` and `object` only, required there | A list's fields of each row, an object's fields: inputs of type `string`, `number`, `bool` or `select`, without `show_if`, `sensitive`, `key` or `per_environment`. |

### Lists, maps and objects

Terraform's complex types get a form of their own, so nobody types JSON:

| Module variable | Input `type` | The form |
|---|---|---|
| `list(object({...}))` of strings, numbers and bools (data disks, firewall rules) | `list` | a row per element, a field per attribute |
| `list(string)`, `set(string)` (DNS servers, IP ranges) | `string_list` | a row per value |
| `map(string)` (tags) | `map` | a row per key, with a value |
| `object({...})` of strings, numbers and bools | `object` | one group of fields |

Rows have **+ Add row** and **Remove**. For a template on a registry module the fields come from the variable's type, in the order written:
`optional(...)` attributes aren't required, and `optional(type, default)`'s default fills a new row. A template can
give a field a label, help, a pattern, a default or dropdown options in `fields`, by name, like an input:

```json
{ "name": "data_disks", "label": "Data disks", "type": "list", "fields": [
    { "name": "lun", "label": "LUN" },
    { "name": "type", "label": "Disk type", "type": "select", "options": ["Standard_LRS", "Premium_LRS"] } ] }
```

Optional fields left empty are left out, so Terraform's `optional()` default applies. Rows left wholly empty are
dropped, and a map key without a value. A `string_list`'s or `map`'s `pattern` checks each value. None of these
can be sensitive, the key or per-environment, nor be what `show_if` refers to; a map or object whose only key is
`sealed` is refused (that's how sealed values are stored). Other complex types (lists of lists, maps of objects,
`map(number)`) aren't form fields yet.

**Strict parsing.** Every object accepts only the fields above: an unknown field (a typo such as `"lable"`) is an error, as is a value of the wrong JSON type. Any invalid file fails the whole catalog: from Git, the last good commit stays loaded and **Settings → Templates** shows the error; from `TEMPLATES_DIR`, the server doesn't start. The error names the file, template, input and field:

```text
storage-account.json: template storage-account, input retention_days: default must be a JSON number (7, not "7")
storage-account.json: template storage-account, input name: unknown field "lable"
```

A catalog has at most 500 files of at most 256 KiB each, 8 MiB in total.

**Editor support.** The format is published as a JSON Schema at <https://opsolete.github.io/Opsolete/schema/template-1.json>. With `"$schema"` in the file, as above, VS Code (and other editors with JSON Schema support) completes field names, shows each field's description, and marks unknown fields and wrongly typed defaults as you type; no extension or setting is needed. The builder writes `"$schema"` into every file it saves. Opsolete itself validates more than the schema can (for example that a `show_if` refers to an input above it), so a file the editor accepts can still fail to load with a clear error.

Mark a `string` or `number` input `"sensitive": true` for passwords and keys. It is typed in a password field, stored encrypted with `SECRET_KEY`, shown as *(sensitive)*, left out of the audit log, and kept when left empty on reconfigure. It can't have a `default`. A module variable declared `sensitive` in Terraform becomes a sensitive input when the template lists it. Values saved before an input was marked sensitive are encrypted the next time the template is configured.

Mark one input `"key": true` (not `bool` or sensitive) to name the template after its value when it's added to a workspace: a key `vm_name` of `Web-Prod 01` adds it as `module.web_prod_01_c8ad`: a random hex suffix, so the same key can be added twice. Without a key, the template's name is used. The name can't be typed in, and is fixed once added: changing the key's value later doesn't rename it, since that would destroy and recreate its resources.

Mark an input `"per_environment": true` for a value that differs per environment (a region, a size): each project may then set it for all its workspaces under *Environment values*, and that value applies at every run, over the workspace's own. See [Environments and promotion]({{< relref "/docs/features/environments#per-environment-inputs" >}}).

Fields generated from a [registry]({{< relref "/docs/features/module-registry" >}}) module's variables take their label and help from the variable's `description`, split once at its first `: ` when the builder imports them: `VM name: 1-64 letters, digits or -.` becomes the label *VM name* and the help *1-64 letters, digits or -.*. So write descriptions in that form. A template's own `help` takes precedence over the module's.

## Converting 0.x templates

Since Opsolete 0.6, only format 1.0 loads; there is no compatibility mode. Three things changed from the 0.5 format:

1. **`default` and `show_if.equals` are typed** by the input's type: a number is a JSON number, a yes / no value a JSON boolean. Strings stay strings.
2. **`help` is its own field.** A `label` is no longer split at `: `; a 0.x label like `"Account name: 3-24 lowercase letters"` would show whole as the label, so move the text after `: ` into `help`.
3. **Unknown fields are errors.** Remove or fix any field not listed above. `"$schema"` is allowed; add it to get editor checks.

Before (0.x):

```json
{
  "id": "vm",
  "name": "Virtual machine",
  "module": { "source": "git::https://example.com/modules.git//vm", "ref": "v1.0.0" },
  "inputs": [
    { "name": "vm_name", "label": "VM name: 1-64 letters or digits", "type": "string", "required": true, "pattern": "^[A-Za-z0-9]{1,64}$" },
    { "name": "disks", "label": "Data disks", "type": "number", "default": "2" },
    { "name": "backups", "label": "Backups", "type": "bool", "default": "true" },
    { "name": "retention", "label": "Backup retention (days)", "type": "number", "default": "7", "show_if": { "input": "backups", "equals": ["true"] } }
  ]
}
```

After (format 1.0):

```json
{
  "$schema": "https://opsolete.github.io/Opsolete/schema/template-1.json",
  "id": "vm",
  "name": "Virtual machine",
  "module": { "source": "git::https://example.com/modules.git//vm", "ref": "v1.0.0" },
  "inputs": [
    { "name": "vm_name", "label": "VM name", "help": "1-64 letters or digits", "type": "string", "required": true, "pattern": "^[A-Za-z0-9]{1,64}$" },
    { "name": "disks", "label": "Data disks", "type": "number", "default": 2 },
    { "name": "backups", "label": "Backups", "type": "bool", "default": true },
    { "name": "retention", "label": "Backup retention (days)", "type": "number", "default": 7, "show_if": { "input": "backups", "equals": [true] } }
  ]
}
```

Loading the 0.x file fails at the first problem it finds, prefixed with the file name. For this file, fixing one after the other:

```text
vm.json: template vm, input disks: default must be a JSON number (2, not "2")
vm.json: template vm, input backups: default must be a JSON boolean (true, not "true")
vm.json: template vm, input retention: default must be a JSON number (7, not "7")
vm.json: template vm, input retention: show_if.equals: each value must be a JSON boolean (true, not "true")
```

The other messages of the format, with `<…>` for your names and values:

```text
template <id>, input <name>: default must be a JSON string ("7", not 7)
template <id>, input <name>: default must be a JSON boolean (true or false), not <value>
template <id>, input <name>: show_if.equals: each value must be a JSON string ("true", not true)
template <id>, input <name>: unknown field "lable"
template <id>, input <name>: option: unknown field "text"
template <id>: module: unknown field "version"
template <id>: unknown field "tags"
```

A label with `: ` in it loads without an error, so search the files for `"label": "` values containing `: ` as well. Opening and saving a template in the [template builder]({{< relref "/docs/features/template-builder" >}}) also writes the 1.0 format, but only once the file loads.

## Writing modules for templates

A template's module is deployed by people who never write Terraform, so it has to stand on its own. Follow these principles:

- **Configure the providers in the module.** Declare `required_providers` and write the `provider` blocks in the module itself, e.g. `provider "azurerm" { features {} }`. The workspace writes no provider configuration, and a template always runs as `module.<name>`, next to other templates. With its own providers, each template works on its own, whatever else the workspace holds.

  Reusable modules called from your own Terraform normally *leave out* provider blocks. A module that has them can't be called with `count`, `for_each` or `depends_on`. Opsolete never writes those arguments, so that limit doesn't apply. Terraform also needs a module's provider configuration to destroy its resources, so removing an added template first runs a destroy targeted at `module.<name>` while its block is still there, and only then drops the block.
- **No credentials in the module.** Pass them as environment variables (`ARM_CLIENT_ID`, `AWS_ACCESS_KEY_ID`, …) through a [variable set]({{< relref "/docs/features/variables" >}}). Environment variables reach every template in the workspace.
- **Resources in the module's root directory**, in the standard module structure (`main.tf`, `variables.tf`, `outputs.tf`), so the [registry]({{< relref "/docs/features/module-registry" >}}) can generate the docs and the form from it.
- **One specific use case, few decisions.** Hard-code what your standards fix (SKU, region list, logging, network rules) and expose only what users must choose.
- **Settings outside the form:** a variable the form doesn't ask can still be set per project with a variable set. Give it a safe default, and take maps as a JSON string (see [Settings outside the template's form]({{< relref "/docs/features/variables#settings-outside-the-templates-form" >}})).
- **Defaults wherever possible**, and a `select` input for variables that have no default, so users pick from approved values instead of typing them.
- **Pin an approved version** in the template's `ref`. Moving the template to a newer one offers existing workspaces a module update, which admins can [roll out]({{< relref "/docs/features/workspaces#update-to-a-new-module-version" >}}) to all of them, in batches and per project. Give inputs that are new and required a `default`, or each workspace needs its update form.

### Writing modules that can import

To [import existing resources]({{< relref "/docs/features/workspaces#import-existing-resources" >}}) the module
must be able to describe them as they are, or the import plans changes, or replaces them:

- Make every setting people choose differently, and that forces replacement when changed (names of sub-resources,
  image, disk type, security settings), an optional input whose default is the module's usual behavior. New
  deployments then stay the same.
- Pass inputs straight through (`attr = var.x` or `coalesce(var.x, …)`), so the import can read which input sets
  which attribute.
- Put values the cloud never returns (`admin_password`, `custom_data`) in `lifecycle { ignore_changes }`.
- Use `count = var.x == "value" ? 1 : 0` for resources that depend on a choice (Windows or Linux VM).
- For parts the scan can't find (Terraform can't list them: data disks, an auto-shutdown schedule, a security
  group's link to a NIC), let the module import them itself: an input `existing` and an `import` block in the
  module (Terraform ≥ 1.16) whose ID it builds from its own inputs; once in state, Terraform skips the block, so
  it stays in the module. The scan then fills those parts in: it tries the part's module with `existing` on for
  each resource of the type the ID names (`…/providers/<namespace>/<type>/${var.name}`; an ID without a name is
  tried once), and what imports fills the form. For that, the module call's arguments come from inputs, a list's
  rows (`each.value.<field>`) or found resources, and required text fields get a placeholder matching their
  `validation`'s `can(regex(...))`, so the provider's own format checks pass:

  ```hcl
  import {
    for_each = var.existing ? toset(["main"]) : toset([])
    to       = azurerm_virtual_machine_data_disk_attachment.main
    id       = "${var.virtual_machine_id}/dataDisks/${var.name}" # azurerm's ID for the attachment
  }
  ```

The [example VM module](https://github.com/Opsolete/terraform-modules-examples/tree/main/virtualmachine) does this:
its data disks (**existing** per disk in `data_disks`), schedule (`auto_shutdown_existing`) and
security group (`nsg_existing`).

## Conditional inputs

Add `"show_if": {"input": "<name>", "equals": ["<value>", ...]}` to an input to show it only while an earlier `select` input has one of the values, or an earlier `bool` input is `true` or `false`. The values are typed like the input they refer to: strings for a `select` (its option values), JSON booleans for a `bool` (e.g. `{"input": "backups", "equals": [true]}`). At least one value is required. In the [template builder]({{< relref "/docs/features/template-builder" >}}) this is a field's **Show when … is …** row. Conditions can be chained (a hidden dropdown hides the inputs that depend on it). A hidden input isn't validated or sent to Terraform, so the module's own default applies. Switching a workspace away from a condition drops that input's stored value, sensitive ones included.

A `select` option is either a string or an object `{"value": "...", "label": "..."}`: the form shows the label and Terraform gets the value.

```json
{ "name": "os", "label": "Operating system", "type": "select", "options": [{ "label": "Windows", "value": "windows" }, { "label": "Linux", "value": "linux" }], "default": "linux" },
{ "name": "admin_password", "label": "Admin password", "type": "string", "sensitive": true, "required": true, "show_if": { "input": "os", "equals": ["windows"] } },
{ "name": "ssh_public_key", "label": "SSH public key", "type": "string", "required": true, "show_if": { "input": "os", "equals": ["linux"] } }
```

Things to know:

- **Module variables need a default.** Every variable behind a conditional input needs one (e.g. `default = null`). Without it, Terraform fails with a missing variable whenever that input is hidden.
- **Registry modules drop unknown inputs.** For a template on a [registry]({{< relref "/docs/features/module-registry" >}}) module, the form follows the module's variables, and JSON inputs the module doesn't declare are dropped. If the dropdown or yes / no input a `show_if` refers to (such as `os`) isn't a module variable, every input that depends on it stays hidden. Add it to the module as a variable.
- **A dropdown without a value counts as its first option**, the one the browser shows selected.

## Where templates live

In production, in a Git repository, so the app keeps no data on its disk:

1. Add a [VCS provider]({{< relref "/docs/features/vcs-providers" >}}) whose token can read the repository.
2. In **Settings → Templates** (admins), choose the connection, repository, branch and folder. The `*.json` files directly in that folder are the catalog.
3. Choose what the [template builder]({{< relref "/docs/features/template-builder" >}}) does on save:
   - **Open a pull request:** a new branch `opsolete/template-<id>-…` with the file and a pull request into the branch. The template changes once it's merged. Saving the template again while its pull request is open adds a commit to that pull request instead of opening another, replacing any change made to the file on its branch (it stays in the pull request's history). Removing a template always opens its own pull request. Works with branch protection.
   - **Commit to the branch:** live right away. The branch must allow direct pushes.
   - **Off:** templates are only edited in Git; a read-only token is enough.

   Saving needs a token with write access: GitHub fine-grained *Contents: read and write* (plus *Pull requests: read and write* for pull requests), Azure DevOps *Code: read & write*. A read-only token fails at the first save, with the provider's error. To give the connection a token with more (or fewer) permissions, use *Change access token* in **Settings → Templates** (the connection's settings): it replaces the token of the whole connection, so its modules and workspaces use it too. If the connection is also open to workspaces, consider a separate connection with its own write token for the catalog.

One app replica checks the branch every minute (GitHub: a conditional request, free when nothing changed), and any on **Sync now**; every replica loads each new commit from Postgres within 10 seconds. A commit with an invalid template isn't loaded: the page shows why, and the last good commit stays. The last loaded files are kept in Postgres, so an instance starts with the templates even while the provider is down.

A save is based on the loaded commit: if the file changed in Git since (GitHub) or the branch moved (Azure DevOps), the provider refuses it and the catalog reloads, so review and save again; nothing is overwritten. That also makes saving from several replicas safe. A VCS connection the catalog uses can't be deleted; **Disconnect** goes back to `TEMPLATES_DIR`.

Without a repository, templates come from `TEMPLATES_DIR` (the repo's `catalog/` in development).
