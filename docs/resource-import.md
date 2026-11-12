# Design: importing existing resources

Status: built (November 2026), checked live on `test-vm`. Not published (outside `docs/content`); the user docs are
[Import existing resources](content/docs/features/workspaces.md#import-existing-resources). Replaces the scan-based
design of 0.7 (in git history: `docs/resource-import.md` before this one).

## Why rebuild

The first design worked out which form values make a template produce an existing resource by **reading the module's HCL and
inverting it in Go** (`registry/adopt.go`, 678 lines), finds resources with `terraform query`, brute-forces
optional parts through the modules' import blocks (`runner/probe.go`), and checks the result with a plan
(`runner/scan.go`). About 1,700 lines with Azure knowledge spread through them (`requireByType`, `probeIDRe`,
`rgOf`, `/subscriptions/` refs, `azurerm_resources`), covering only the expression shapes someone coded.

Terraform already does each of those jobs: finding (data sources), importing (import blocks in modules, ≥ 1.16),
mapping attributes to variables (the plan JSON's `configuration` references) and judging the fit (the plan).
Opsolete should be the GUI around that, not a second interpreter.

A tried and dropped middle way: tracing the module with marked inputs and solving the marks against what exists
(spike, November 2026). It filled 21 inputs of `test-vm` without knowing the module, but needed an ARM-to-azurerm
type table, recipes for composite IDs and fallbacks for every format the provider checks: another engine.

## Design

**The module imports itself; the plan is the check; Opsolete copies what the plan found into the form.**

### Contract for modules

A module is importable when an input (by convention `existing`, a bool, default false) makes it import what
exists instead of creating it: `import` blocks switched by that input, their IDs from the inputs and from data
sources. Plain Terraform (≥ 1.16: import blocks in modules); the provider knowledge (ID formats, how to find a
VM's NIC) stays in the module, where the first design's review already put it. The template needs nothing.

```hcl
variable "existing" {
  type    = bool
  default = false
}

data "azurerm_resources" "vm" {
  count               = var.existing ? 1 : 0
  name                = var.name
  resource_group_name = var.resource_group_name
  type                = "Microsoft.Compute/virtualMachines"

  lifecycle {
    postcondition {
      condition     = length(self.resources) == 1
      error_message = "existing: there is no VM named ${var.name} in resource group ${var.resource_group_name} to import."
    }
  }
}

import {
  for_each = var.existing ? toset(["vm"]) : toset([])
  to       = azurerm_windows_virtual_machine.main
  id       = data.azurerm_resources.vm[0].resources[0].id
}

# The VM doesn't report its NICs; a NIC reports its VM.
data "azurerm_resources" "nics" {
  count               = var.existing ? 1 : 0
  resource_group_name = var.resource_group_name
  type                = "Microsoft.Network/networkInterfaces"
}

data "azurerm_network_interface" "found" {
  for_each            = toset(var.existing ? data.azurerm_resources.nics[0].resources[*].name : [])
  name                = each.key
  resource_group_name = var.resource_group_name
}

locals {
  found_nic = one([for n in data.azurerm_network_interface.found : n if lower(n.virtual_machine_id) == lower(data.azurerm_resources.vm[0].resources[0].id)])
}

import {
  for_each = var.existing ? toset(["nic"]) : toset([])
  to       = module.nic.azurerm_network_interface.main
  id       = local.found_nic.id
}
```

The VM is found through `azurerm_resources` (any type, by name), not the feature-frozen `azurerm_virtual_machine`
family. The example is `windows-vm` 2.0.0 (terraform-modules-examples), one template per OS: the earlier
`virtualmachine` with its Windows/Linux switch is gone, and `linux-vm` gets the same import later. Checked live
again with this lookup: `7 to import, 0 to add, 0 to change, 0 to destroy`; an unknown name fails on the
postcondition.

Optional parts keep their own switches (`data_disks[].existing`, `auto_shutdown_existing`, `nsg_existing`), as
the example modules 1.1.0 already have them.

### Flow

1. **Add the template with `existing` on.** The usual add (or deploy) form: `existing` is a module variable, so
   it's an input like any other (`app.ImportInput`); for templates that have it (`app.CanImport`) the form offers
   *Create new* / *Import existing* at the top (`?import=1` ticks it, and the form marks only the key input
   required, `app.ForImport`). While it's on, only the key input is required (`app.validate`):
   the user fills what identifies the resource (name, resource group); a required input left empty is stored as
   `null`, so the module call still has the argument.
2. **Plan.** An ordinary apply run. Terraform finds and imports; the plan lists every attribute that differs, real
   value first. The runner reports the plan's imports (`Plan.Imports`, IDs from `importing`). An apply run's plan
   that imports an ID a state holds (this workspace's as the plan read it, the others' index) → errored
   (`importConflict`), whatever imports it. One that imports into a template's module (`intoTemplate`) always
   waits for confirmation, also with auto-apply, and replaces or deletes nothing, else errored; a VCS
   configuration's own import blocks are left to its authors. Errored, the template change is undone like any run
   that ends before its apply.
3. **Found values.** The runner reads the plan JSON (`runner/found.go`): for every imported resource, the real value
   (`change.before`) of each attribute one input sets alone, following the `configuration` references up through
   module calls (`each.value.x` of a call repeated over a list input: that row's field, the row matched by the
   instance key). References can't tell `var.x` from `"${var.x}-nic"`, so an input already set must also be what
   the plan sets there. Sensitive attributes and variables and workspace secrets are left out (again on the server).
   Stored per module with its template (`runs.found`, migration 008).
4. **Confirm, or Change settings.** An import plan's page always offers both to operators: *Change settings*
   (`POST …/runs/{run}/change`, `app.ChangeImport`) discards a plan still waiting and redirects to the configure
   form (`?found=<run>`), or the add form again when the run's restore removed the template it added
   (`?template=…&found=<run>&from=<module>`); from an ended plan it only redirects. Any run that changed a template
   and ends without its apply (a failed plan too) keeps what it set the template to, sealed inputs left out
   (`storage.keepEntered`, before the restore), so Change settings works there too. The form opens with the found values over the inputs that plan ran
   with (stored with them, `Found.Saved`: what the user set by hand, such as a NIC name the module builds from two
   inputs, isn't lost when the discard removes the template), `existing` as that run had it (a plan that failed
   creating comes back creating); an applied run offers nothing to go back to, also by an old `?found` link; saving plans again, these values
   counting as the workspace's own against a project's keep-own value. Live (`test-vm`): round 1 found 15
   settings; round 2 left the NIC name and boot diagnostics; with the NIC name set, round 3 left boot diagnostics
   only (`2 to import, 0 to add, 1 to change, 0 to destroy`).
5. **Fix the rest by hand.** What the module builds from several inputs (`coalesce(var.nic_name,
   "${var.name}-nic")`) or a dynamic block isn't copied: the plan shows it, the user sets it. Repeat until
   `N to import, 0 to add, 0 to change, 0 to destroy`, then confirm.
6. **Applied: the switch goes off.** The apply's finishing transaction sets `existing` false in the templates the
   plan imported into (`storage.endImport`). That changes no plan (the resources are in the state, so the import
   blocks did nothing anymore, and the data sources stop being read); a resource deleted outside Terraform later
   is then created again instead of failing its lookup, and a promotion doesn't copy the switch.

Decision (November 2026): the scan (inventory, matching, Import page) is removed, not kept as a picker. A picker
can come back on `terraform query` if wanted.

### Live check (November 2026, `test-vm`, Terraform 1.17.1, azurerm 4.81, reader credentials, plans only)

| Plan | Inputs | Result |
|---|---|---|
| 1 | `name`, `resource_group_name`, `existing` | VM and NIC (`test-vm235`, portal-made) found and imported; plan lists the differences |
| 2 | + 15 found values (location, subnet, public IP, size, image, disk type, Trusted Launch, IP configuration, …) | `2 to import, 1 to add, 1 to change, 1 to destroy`: only the NIC name and boot diagnostics left |
| 3 | + `nic_name`, `boot_diagnostics_enabled` by hand | `2 to import, 0 to add, 0 to change, 0 to destroy` |
| 4 | + schedule, NSG and data disk switches with rough values | `7 to import, 0 to add, 3 to change`; found values filled the schedule's time, time zone and email |
| 5 | + the data disk row's size, type and caching | `7 to import, 0 to add, 0 to change, 0 to destroy` (the first design's result) |

The prefill was a 50-line Python spike over the plan JSON, without knowledge of Azure or of the module; built as
`runner/found.go`, which adds the pass-through check and list rows.

## What changed in the code

| 0.7 | now |
|---|---|
| `registry/adopt.go`, `runner/scan.go`, `runner/probe.go`, `app/imports.go`, the scan wire types | deleted |
| scan runs, Import page, `POST /workspaces/{name}/scans` | removed (migration 008 deletes scan runs) |
| `runs.imports`, import blocks written by Opsolete | the module's import blocks; `Plan.Imports` feeds the guard |
| form filled from a scan (`?scan&n`) | `?found=<run>` from the plan JSON (`runner/found.go`, `runs.found`) |
| random placeholders for ignored secrets | gone: the user types a value (any, if the module ignores it; else the plan shows it changes) |

Kept: the import guards, `Plan.Import` counts, keep-own environment values.

## Limits

- Modules have to be written for it (import blocks, data sources, Terraform ≥ 1.16). Modules without an
  `existing` input simply don't offer the switch.
- Optional parts aren't detected: the user switches on the schedule, NSG or a data disk row. A module may detect
  them with data sources where the provider has them.
- Values built from several inputs are fixed by hand, guided by the plan.
- Attributes the provider treats as unmanaged when unset (`security_rule` with `nsg_rules = []`) fit without
  being managed: portal-made NSG rules stay unmanaged until they're entered as rows.
- Not tried: Linux VMs; a NIC outside the VM's resource group (the module above only looks there).

## Open

- The example modules: `existing` for the VM itself (the data source and import blocks above); in
  [terraform-modules-examples](https://github.com/Opsolete/terraform-modules-examples), not this repository.
- Not tried: Linux VMs; a NIC outside the VM's resource group; `runner/found.go` itself against Azure (the spike
  was; the Go version is tested on plan JSON shaped like Terraform's, and with a real Terraform import).
- A picker (what exists, what's managed) on `terraform query`, if wanted.

## Research (November 2026)

- Terraform: import blocks (1.5), `for_each` on import (1.7), resource identity (1.12), `list` blocks and
  `terraform query` (1.14), import blocks in child modules (1.16; 1.16.0 drops some imports on `for_each`
  instances, use ≥ 1.16.1).
- Closest products: Pulumi's program-first import (plan the program, plug in IDs), Azure Landing Zones' migration
  guide (import blocks at module addresses, edit the variables until the plan is clean), StackGuardian and Firefly
  (AI-filled module inputs, plan until the diff is zero). This design is that loop with Terraform doing the finding and
  the plan JSON doing the filling.
