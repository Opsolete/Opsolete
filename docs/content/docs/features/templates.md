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
| `adopt` | array | optional | Resources of the module that can take over existing ones; see [adopting existing resources](#adopting-existing-resources). |

**Input:**

| Field | Type | | |
|---|---|---|---|
| `name` | string | required | The Terraform variable it sets: `^[a-z_][a-z0-9_]*$`, not `source`, `version`, `providers`, `count`, `for_each`, `lifecycle`, `depends_on` or `locals`. Unique in the template. |
| `type` | string | required | `string`, `number`, `bool` (a checkbox) or `select` (a dropdown). |
| `label` | string | optional | The question the form asks. Empty: the form shows the name. |
| `help` | string | optional | Shown under the field (and linked to it for screen readers), and the error message when a value doesn't match `pattern`, so say there what fits. Without help, the error shows the pattern. |
| `required` | boolean | optional | The value can't be empty. Dropdowns and checkboxes always have a value. A hidden input is never required. |
| `pattern` | string | optional | A Go (RE2) regular expression the whole value must match. The browser checks it too, as a JavaScript one. |
| `options` | array | `select` only, required there | One entry per choice: a string (shorthand for `{"value": "<string>"}`) or `{"value": "...", "label": "..."}`: the form shows `label`, Terraform gets `value`. `value` is required and not empty. |
| `default` | typed | optional | Filled in when the form opens. Typed by `type`: `string` and `select` a JSON string (a select's must be one of its option values), `number` a JSON number (`7`, `0.5`), `bool` a JSON boolean (`true`, `false`). It must pass the input's own checks (`pattern`). |
| `sensitive` | boolean | optional | `string` and `number` only, no `default`; see below. |
| `show_if` | object | optional | `{"input": "<name>", "equals": [...]}`; see [conditional inputs](#conditional-inputs). |
| `key` | boolean | optional | At most one input, not `bool` or sensitive; see below. |
| `per_environment` | boolean | optional | Not with `sensitive` or `key`. Each project may set the value for all its workspaces; see below. |

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
- **Settings outside the form:** a variable the form doesn't ask can still be set per project with a variable set. Give it a safe default, and take maps and lists as a JSON string (see [Settings outside the template's form]({{< relref "/docs/features/variables#settings-outside-the-templates-form" >}})).
- **Defaults wherever possible**, and a `select` input for variables that have no default, so users pick from approved values instead of typing them.
- **Pin an approved version** in the template's `ref`. Moving the template to a newer one offers existing workspaces a module update, which admins can [roll out]({{< relref "/docs/features/workspaces#update-to-a-new-module-version" >}}) to all of them, in batches and per project. Give inputs that are new and required a `default`, or each workspace needs its update form.

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

## Adopting existing resources

Resources built by hand (in the Azure portal, by scripts) can be brought under a template without being created again.
List the module's resources that can do that in `adopt`:

```json
"adopt": [
  { "to": "azurerm_windows_virtual_machine.self[0]", "label": "VM resource ID",
    "pattern": "(?i)/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Compute/virtualMachines/[^/]+",
    "help": "The VM's Resource ID, from Properties in the Azure portal.",
    "show_if": { "input": "os_type", "equals": ["windows"] } },
  { "to": "azurerm_network_interface.self", "label": "Network interface resource ID",
    "pattern": "(?i)/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Network/networkInterfaces/[^/]+" }
]
```

| Field | Type | | |
|---|---|---|---|
| `to` | string | required | The resource's address in the module, e.g. `azurerm_linux_virtual_machine.this` or `azurerm_windows_virtual_machine.self[0]` (with `count`). Unique. |
| `label` | string | optional | The field's title. Empty: the address. |
| `help` | string | optional | Shown under the field, and the error when the ID doesn't match `pattern`. |
| `pattern` | string | optional | A Go (RE2) regular expression the whole ID must match, e.g. to tell a VM ID from a NIC ID. Start it with `(?i)`: Azure isn't consistent about casing. Without one, an `azurerm_` resource's ID must have Azure's shape (`/subscriptions/…/resourceGroups/…[/providers/…]`). |
| `show_if` | object | optional | As for inputs, e.g. for a resource that exists only for one `os_type`. |

When the template is added (to a new workspace or an existing one), the form has an **Adopt existing resources**
section with one field per shown entry. With all fields empty the template creates its resources as usual. With any
ID filled in, every shown field needs one, and the user types the resource's **current** settings into the inputs
above. The run then imports the resources into `module.<name>`:

- The plan lists them as **import** ("1 to import, 0 to add, 0 to change, 0 to destroy" when the template matches
  the resource exactly). Changes in the same plan show where the inputs or the module differ from what exists.
- It always waits for confirmation, even with auto-apply.
- If it would replace or delete anything, the run ends as errored and nothing is applied: the template doesn't fit
  the resource. Added to an existing workspace, the template is removed again.
- After the apply the resources are ordinary template resources. Configuring the template later asks for no IDs.

### Generate from module

For a template on a [registry]({{< relref "module-registry" >}}) module, **Generate from module** in the
[builder]({{< relref "template-builder" >}}) writes the `adopt` entries below from the module's code, ready to
review: each managed resource becomes an entry found with its own type; `count = var.x == "v" ? 1 : 0` gives its
`show_if` (and makes `x` a dropdown); `attr = var.x` and `coalesce(var.x, …)` fill `inputs`, also in nested blocks;
`dynamic "b"` with `for_each = var.x ? … : []` maps the yes / no input `x`; `[other.res.id]` makes `other` a
related entry (`id_from`); a sensitive input in `lifecycle { ignore_changes }` becomes a `placeholder`. Other
expressions, `for_each` resources and nested modules are skipped and named in the result. For the
[example VM module](https://github.com/Opsolete/terraform-modules-examples/tree/main/virtualmachine) it writes the
three entries a scan needs (Windows VM, Linux VM, network interface).

### Scanning: found and filled in

A template whose `adopt` entries also say how to find resources lets users skip the pasting: **Deploy → Import
existing resources** scans the cloud with the workspace's credentials, lists what the template can adopt, and
**Adopt** opens the form with every value and ID filled in. Add to the entries:

| Field | |
|---|---|
| `list` | The provider's list resource type that finds the entry's resources with `terraform query`, e.g. `azurerm_windows_virtual_machine` ([which types support it](https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs)). An entry with `list` and without `id_from` finds the candidates; its one-value `show_if` sets that input (e.g. `os_type` = `windows`). |
| `require` | Only resources whose attribute is set count. azurerm's Linux VM list also returns Windows VMs: use `provision_vm_agent` for Windows, `disable_password_authentication` for Linux. |
| `id_from` | For a related resource: the candidate's attribute holding its ID, e.g. `network_interface_ids[0]`. With `list` too, its attributes fill inputs. |
| `inputs` | `{"<input>": "<attribute path>"}`, e.g. `{"size": "size", "os_disk_type": "os_disk[0].storage_account_type"}`. A yes / no input takes whether the attribute is set (e.g. `"boot_diagnostics_enabled": "boot_diagnostics"`). |
| `placeholder` | Sensitive inputs filled with a random value when adopting, for values the cloud never returns (an admin password the module ignores with `ignore_changes`). The form doesn't require them then. |

A scan is a run (action `scan`) on the workspace: the runner lists the resources (azurerm ≥ 5.7 for VMs; types the
provider can't list, such as managed disks, are left out of the search and their entries only imported by ID), fills
in the form values, and plans adopting all of them, each as its own module, into a throwaway state. Nothing is
written or locked. The run page shows each one as **fits** (imports only), **differs** (and which attributes would
change), **doesn't fit** (would replace it: no Adopt), **incomplete** (the form lacks values; Adopt and fill them
in) or **managed** (a workspace's state has its ID already). Adopting an ID a workspace manages is refused, also when
pasted by hand: a resource can be in one state only. The adoption's plan checks it again against the state as it
reads it, and ends the run as errored if the resource is there already. States record resource IDs since this feature; an older one
gets them at its workspace's next run (any run but a speculative plan or drift check, even one without changes). Resource IDs take the case of the input values
(Azure reports `TEST1` in a VM's ID and `test1` in its NIC's). Found values never include sensitive attributes'
values: map only plain settings. At most 50 resources per scan are checked; the rest are listed as unchecked.

**Modules that can adopt.** Portal-made resources rarely match a module's fixed choices, and a different value of a
setting that forces replacement makes the plan replace the resource. Make such settings optional inputs whose default
is the module's current behavior (names of sub-resources such as the NIC, image, OS disk type, secure boot and vTPM),
write blocks Azure always reports explicitly (`additional_capabilities {}`), and put values Azure never returns in
`ignore_changes` (`admin_password`, `custom_data`). Use the casing of the resource ID for names such as
`resource_group_name`: a different casing forces replacement.

The [template builder]({{< relref "template-builder" >}}) edits `adopt` under **Adopt existing resources**.

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
