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

### Flow (an import session, November 2026; replaces rounds of discarded apply plans)

1. **Import existing.** The add (or deploy) form's *Create new* / *Import existing* chips set the form's mode
   (`app.Importing`, posted as the hidden `existing`); only the key input (if the template has one) and required sensitive inputs stay required (`app.ForImport`, a required
   input left empty is stored as `null`). Saving makes a configuration version whose module **imports**
   (`configuration_modules.importing`, migration 014; the switch is never stored as an input) and queues a **plan
   round**: a run with action `plan` (`app.runAction`), never applied, no state lock, the workspace unchanged. The
   runner gets `existing = true` for importing modules only (`app.assembleJob`, if the template at the pinned
   version has the switch).
2. **The round's page** (`app.ImportRound`, operators): `N to import · add · change · destroy`, the plan's
   attribute diff (step 4b), and per importing module the inputs one setting sets directly, found (`runner/found.go`,
   type-checked against the input in `app.fits`: a map behind a string input is dropped) next to planned with.
3. **Plan again with the found values** (`POST …/plan-again`, `app.PlanImportAgain`): a new version, the found values
   saved through the one input pipeline (validated, sealed, keep-own), another round. **Change settings** opens the
   form with what was entered and found, still importing; saving queues a round too.
4. **Apply the import** (`POST …/adopt`, `app.AdoptImport`): offered for the workspace's latest round when it finished
   with imports and replaces or deletes nothing. It queues an apply run of exactly that round's version
   (`storage.AdoptRun`): `afterPlan` makes it wait (reason `import`), `FinishJob` errors it if its plan replaces or
   deletes, and `assembleJob` checks the import guard again when the apply is claimed (`ApplyImportConflict`), since
   another workspace may have imported the resource since the round.
5. **Applied:** the apply's finishing transaction makes a version with the modules it imported into no longer
   importing (`storage.endImport`). Every round of that version, and the apply, then offer nothing to go back to
   (`RunVersionApplied`), also by an old `?found` link.

A plan round that has nothing to change leaves the workspace's version alone (only applying runs take a version:
`runFacts.Version`).

Decision (November 2026): the scan (inventory, matching, Import page) is removed, not kept as a picker. A picker
can come back on `terraform query` if wanted.

### Rules that keep a session safe (November 2026)

- **A round is pending** (`storage.RunPending`) while its configuration isn't applied and is the workspace's current
  version or was made from it. Only a pending round offers Plan again, Change settings and Adopt, and storage checks
  it again inside the transaction (`ErrChanged`): going on from a round older than another applied change would undo
  that change (a removed template back, another template's settings reverted, importing switched on again).
- **Adopt** takes the latest round: a later run that plans another configuration supersedes it. Drift checks, pull
  request plans and the runs of the same version (a failed adopt) don't, so they never block the session.
- **The guard compares the imported object's own `id`** (the plan's `before.id`, else the import id): what state
  indexes hold. An import Terraform names no id for (by identity) is refused, as is the same id imported twice in one
  plan: Opsolete can't otherwise promise one resource, one state.
- **Inputs the project sets** (per environment, not keep-own), or hidden by `show_if`, show what was found but never
  ask for another round: planning again would set them back (a loop).
- **Retention** keeps pending rounds: the session lives on them.
- `internal/e2e` runs a real session: a module that imports itself (`examples/modules/importable`, `terraform_data`,
  offline), a round, adopt, confirm, apply, and a second workspace refused for the same resource.

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
- **Forget instead of destroy** when removing a template that holds imported resources (Terraform `removed` blocks
  with `destroy = false`): today removing it destroys them, behind the usual confirmation. Needs a check with real
  Terraform of a module that configures its own provider.
- Explicit attribute → input mappings in templates, if the type-checked inference proves too loose.
- Imports by resource identity (Terraform 1.12+) have no ID, so the one-state guard can't check them.

## Research (November 2026)

- Terraform: import blocks (1.5), `for_each` on import (1.7), resource identity (1.12), `list` blocks and
  `terraform query` (1.14), import blocks in child modules (1.16; 1.16.0 drops some imports on `for_each`
  instances, use ≥ 1.16.1).
- Closest products: Pulumi's program-first import (plan the program, plug in IDs), Azure Landing Zones' migration
  guide (import blocks at module addresses, edit the variables until the plan is clean), StackGuardian and Firefly
  (AI-filled module inputs, plan until the diff is zero). This design is that loop with Terraform doing the finding and
  the plan JSON doing the filling.
