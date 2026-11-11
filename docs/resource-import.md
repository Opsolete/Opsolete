# Design: importing existing cloud resources

Status: built on branch `feat/resource-import` (PR #34). Not published (outside `docs/content`); the user docs are
[Import existing resources](content/docs/features/workspaces.md#import-existing-resources).

## Problem

Teams have infrastructure built by hand (Azure portal, scripts) in no Terraform state: mostly VMs, plus Key Vaults,
storage accounts and databases. They want to manage it with Opsolete, through the same no-code template forms as
everything Opsolete creates, **without redeploying it**, and without Terraform, Git or CI/CD knowledge.

## Design

**A workspace's Import page**, separate from templates:

1. **Scan** (optionally one resource group): a run with action `scan` and the workspace's credentials. Plan phase
   only, no backend, no lock, never applied. It lists every resource (Azure: resource groups by `terraform query`,
   then `azurerm_resources` per group) and what the templates can import.
2. **Results** in four groups: *can be imported* (fits / differs + the attributes / incomplete, from a plan importing
   all of them, each as its own module, into a throwaway state), *template needs work* (the plan would replace it),
   *no template* (its type), *already managed* (the workspace). Resources a candidate imports or refers to (NIC, OS
   disk, public IP) are listed with it.
3. **Import**, one at a time: the template's add form, filled in from the scan; the plan imports the resources
   (IDs from the stored scan result, never the browser), always waits for confirmation.

**Templates need nothing for it.** What a template can find and how that fills its form is read from its registry
module's code at scan time (`registry/adopt.go`, HashiCorp's HCL parser): every managed resource is searched by its
type; `count = var.x == "v" ? 1 : 0` → exists for x = v; `attr = var.x` / `coalesce(var.x, …)`, nested blocks too →
input x; `dynamic "b" { for_each = var.x ? … : [] }` → yes/no input x; `[other.res.id]` → other is related
(`id_from`); sensitive input in `ignore_changes` → random placeholder. Module calls are followed (local, and this
registry's newest version within the constraint), values through arguments, locals and outputs. Optional and
repeated parts (`count = var.x != null ? 1 : 0`, or `for_each` over a list input's rows, on a module call) whose
module imports existing ones itself (import blocks switched by an input, e.g. `existing`, Terraform ≥ 1.16) are
probes (`runnerapi.Probe`): where each call argument comes from (an input, a row field, a found resource's ID),
which one switches the import and which takes the name, the type the import ID names (`/providers/<ns>/<type>/`
before the name), and which attribute fills which input, row field or list (dynamic blocks). The runner
(`runner/probe.go`) tries them in one throwaway plan per scan, a module per try calling the part's module directly:
per inventory resource of that type in the candidate's resource group, or once. A failing import fails only its
try (the errored plan file still has the others); a try whose every import succeeds belongs, its "before" fills
the form with the switch on, and the candidate is validated again before the fit check. What the module imports
itself is the candidate's `found`: checked against the states like its imports, never written as import blocks.
Required text fields get a placeholder matching their pattern (the provider checks formats before importing). Live:
`vm-test` fills its data disk, schedule and NSG with its rules, fit `7 to import, 0 to add, 0 to change, 0 to
destroy`. No naming convention: the code says which
input sets which attribute. A small table covers azurerm's Linux VM list also returning Windows VMs (`require`).

Rejected:
- *Keeping Terraform's generated code* (aztfexport style): doesn't plan without hand edits, needs Terraform users.
- *Describing adoption in the template* (`adopt` entries, builder section; built first, then removed): ~40 fields per
  template, all derivable from the module, and it made templates feel like import configuration.
- *Variable name = attribute name convention*: fragile (shared variables, nested blocks, `coalesce`), and existing
  modules wouldn't fit.
- *Opsolete writing the parts' import blocks* (instead of the modules' `existing` switches, review November 2026):
  the IDs need the provider's formats (a data disk attachment's is `<vm id>/dataDisks/<name>`), which the module
  knows and Opsolete shouldn't. The modules keep them.
- *Reading friendly inputs back through tables* (`local.images[var.image].sku` → `image`, built and dropped in
  review): another code shape to match, the list twice (module and template), and Opsolete reading a module's
  private locals. Raw inputs (publisher, offer, SKU) import as they are; a template makes them dropdowns.
- *Terraform variables setting fields the form leaves off* (built and dropped in review): a workspace variable
  bypassed the template's options and patterns. A project's subnet is a per-environment value, with *keep own*
  for the workspaces (and imports) that have their own.

## How it works

- **Server** (`app/imports.go`): per catalog template on a registry module, read the archive and derive the entries;
  templates on `git::` modules or without findable resources are listed as skipped (Import page, scan log). The job
  carries the templates (inputs JSON), the entries and each template's module (`runnerapi.Scan`).
- **Runner** (`runner/scan.go`): one `terraform query -json` with `include_resource` for all list types (attributes
  as JSON; types the provider can't list are dropped and queried again), filtered by `resource_group_name`; per
  template, candidates from the primary entries, related ones through `id_from`, inputs filled, IDs given the case
  of the input values (Azure: `TEST1` in a VM's ID, `test1` in its NIC's: a different case of `resource_group_name`
  forces replacement), validated like the form; then the fit plan (`fitPlan`). Sensitive values never leave it.
- **Storage**: `runs.scan_resource_group`, `runs.candidates` (migration 005); `runs.imports` (004), written as import
  blocks in both phases; resource IDs in the state index (`state_version_resources.resource_id`), indexed at the plan
  phase of locking runs too.
- **Guards**: importing an ID a state manages is refused (index), and again at the end of the import's plan (the
  state the plan read + other workspaces' index); a plan that replaces or deletes ends the run errored. Either way
  the added template is removed again.
- **Plans count imports** (`Plan.Import`, change action `import`): before, an import without changes showed as
  "0 to add, 0 to change, 0 to destroy".

## Spike and live check (November 2026, test subscription)

- `terraform query -json` with `include_resource` returns each resource's attributes as JSON in ~1 s.
- azurerm 5.13 lists Linux/Windows VMs, NICs, storage accounts, Key Vaults, SQL, resource groups; not PostgreSQL
  Flexible Server or managed disks.
- The portal-made Windows VM `testvm` imported with the example VM module 3.1.0: `2 to import, 0 to add, 0 to change,
  0 to destroy`, applied, next plan without changes. The scan filled every input as typed by hand before.
- Module requirements (now in the template docs): optional inputs for portal-made settings (NIC name, image, disk
  type, Trusted Launch, boot diagnostics), `ignore_changes` for values Azure never returns.
- Optional parts (data disks, auto-shutdown, NSG): azurerm can't list them, and only its legacy
  `azurerm_virtual_machine` shows a VM's data disks (a maintainer: "needs to be removed in a future major release").
  Tried and dropped: reading them through that legacy type (first version), `azapi` (a reference graph of Azure's
  raw JSON found every link, but adds a provider), Terraform policy (`core::getresources` only sees what list blocks
  found, and its engine needs an HCP entitlement). Kept: import blocks in the modules (Terraform 1.16): the portal-made
  `vm-test` (data disk, auto-shutdown, NSG with an RDP rule) with `virtualmachine` 4.1.0, values typed by hand:
  `7 to import, 0 to add, 0 to change, 0 to destroy`, applied, next plan without changes; a 4.0.0 deployment
  upgrades with moves only.

## Live check (November 2026, after the review)

- The scan with `virtualmachine` 5.0.0 on the portal-made `test-vm` (subscription-wide, 3 resource groups): 34 s;
  the VM with its NIC, data disk and attachment, schedule, NSG and its association; fit `7 to import, 0 to add,
  0 to change, 0 to destroy`. `terraform query` itself: 3 s.
- A project subnet without *keep own*: the import plan moves the NIC to it **in place** (`1 to change`), which the
  replace guard doesn't stop: the form now warns when a project value differs from what was found. With *keep own*:
  `7 to import, 0 to change`.
- The one-pipeline idea (below), tried by hand: an import plan's "before" equals the query's attributes (58 of 58,
  but write-only ones), and azurerm refuses a Windows VM imported as Linux by itself (no `requireByType` needed). But
  it took 9 s against the query's 3, the NIC needs a second round (its ID comes from the VM's attributes), and a
  primary resource has no import block saying which cloud type to try from the inventory: Opsolete would need a
  table of azurerm types to Azure types, provider knowledge it now gets from `terraform query`. Not worth it: kept
  as it is.

## Open

- One fill pipeline (inputs from `terraform query`, parts from the probe plan's "before"): checked live, not worth
  it (see above).

- Linux VMs, extensions: not tried on real resources.
- Probes: more than 100 tries per scan are skipped (logged); a part from a local module (`./x`) isn't tried.
- More than 50 found per scan: fit-checked in batches; paging past `limit = 1000` per list type.
- Resources no template can manage: listing them would show which templates to write next.
- Import by resource ID (without a scan), bulk import of everything that fits.
