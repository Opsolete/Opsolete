# Design: adopting existing cloud resources (resource import)

Status: phases 1 and 2/3 (scan, pre-fill, fit check) built on branch `feat/resource-import` (PR #34, draft).
Not published (outside `docs/content`).

## Problem

Teams have infrastructure that was built by hand (Azure portal, scripts) and is not in any Terraform state:
mostly VMs, plus Key Vaults, storage accounts and SQL / PostgreSQL databases. They want to manage it with
Opsolete, through the same no-code template forms as everything Opsolete creates, **without redeploying it**.
The people doing this work don't use Terraform, Git or CI/CD.

## Approach

Adopt resources **into templates**: an existing resource becomes a template added to a workspace
(`module.<name>`), exactly like one Opsolete created. Terraform does all cloud work on the runners (they already
get the credentials as workspace variables); the server stays cloud-neutral and holds no cloud SDK.

1. **Scan** (phase 3): find what exists. `terraform query -json` with `list` blocks (Terraform ≥ 1.14, azurerm ≥ 5.7 for
   VMs); the `azurerm_resources` data source for types without list support.
2. **Read** (phase 2): fetch a resource's current settings. `import` blocks + `plan -generate-config-out`; the
   template maps the read attributes to its inputs, so the form arrives pre-filled.
3. **Adopt** (phase 1): plan the template with `import` blocks into `module.<name>`. The plan says whether the
   template fits the resource: 🟢 imports only, 🟡 also updates (the form or the module differs from reality),
   🔴 replaces or deletes (refused).
4. **Fit report** (phase 3): the 🟡 / 🔴 reasons summed over all scanned resources tell module authors which inputs
   to add next.

Rejected: keeping Terraform's generated configuration as the managed code (aztfexport style). It doesn't plan
without hand edits (conflicting arguments, see the spike) and needs people who edit Terraform.

## Spike results (November 2026, test subscription, one Windows VM)

- `terraform query -json` (1.3 s) found resource groups, VMs and NICs as JSON lines
  (`type: list_resource_found`, with the resource's identity: name, resource group, subscription).
- azurerm 5.13 has list resources for Linux/Windows VMs, NICs, storage accounts, Key Vaults, SQL server/database
  and resource groups; **not** for PostgreSQL Flexible Server or managed (data) disks.
- The Linux VM list also returns Windows VMs. The OS comes from the OS disk: `data "azurerm_managed_disk"` →
  `os_type`.
- `-generate-config-out` reads every attribute; the NIC is reachable from the VM's `network_interface_ids`.
  Passwords and `custom_data` read as null (Azure never returns them).
- Generated code doesn't plan as is (conflicting arguments): only its values are useful.
- Azure's casing varies (`TEST1` in the VM's identity, `test1` in IDs); a different casing of
  `resource_group_name` forces replacement. Use the casing of the resource ID.
- Adopting the VM with the example VM module unchanged: 🔴 2 to destroy (NIC name, RG casing, secure boot / vTPM,
  OS disk type, image). After making those optional inputs (defaults = old behavior), plus `boot_diagnostics`,
  `additional_capabilities {}` and `ignore_changes = [admin_password]`: 🟢 `2 to import, 0 to add, 0 to change,
  0 to destroy`. `-detailed-exitcode` is 2 and the plan JSON has `actions: ["no-op"]` with `importing` set.

### Writing modules that can adopt

- Every value that forces replacement and that people set differently in the portal (names of sub-resources,
  image, disk type, security settings) is an optional input whose default is the module's current behavior.
- Blocks Azure always reports (`additional_capabilities`) are written explicitly.
- Values Azure never returns (`admin_password`, `custom_data`) are in `ignore_changes`.

## Phase 1: adopt with pasted IDs

A template may list the module's resources that can be adopted. When adding the template (to a new or an existing
workspace) the user may paste their resource IDs and types the current settings into the form.

Template file:

```json
"adopt": [
  { "to": "azurerm_windows_virtual_machine.self[0]", "label": "VM resource ID",
    "pattern": "(?i)/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Compute/virtualMachines/[^/]+",
    "show_if": { "input": "os_type", "equals": ["windows"] } },
  { "to": "azurerm_network_interface.self", "label": "Network interface resource ID",
    "pattern": "(?i)/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.Network/networkInterfaces/[^/]+" }
]
```

- `to`: the resource's address inside the module; the import goes to `module.<name>.<to>`.
- `pattern`: the ID's format (whole value, Go regexp). Cloud-neutral: Opsolete knows no Azure types.
- `show_if`: as for inputs (e.g. the VM resource depends on `os_type`).
- All IDs empty: nothing is adopted, the template creates resources as before. Any given: every shown entry needs one.
- The template builder edits these entries (section 4): it suggests a registry module's resources as addresses
  and common Azure ID patterns.

Flow:

1. The add / create form shows "Adopt existing resources" with one field per shown entry.
2. The server checks the IDs (pattern, length, no line breaks) and queues the template's apply run with
   `runs.imports = [{to, id}]` and `always_confirm` (auto-apply never applies an adoption).
3. The runner writes `opsolete_imports.tf.json` (IDs escaped like inputs: they are strings in `.tf.json`) next to the
   module blocks; Terraform plans the imports.
4. The plan summary counts imports (`Plan.Import`, change action `import`); imports with no changes used to be
   invisible ("0 to add, 0 to change, 0 to destroy").
5. **Guard**: an adoption plan that replaces or deletes anything ends the run as errored, nothing applied. Adding a
   template to an existing workspace is then undone (the run ended before its apply); a workspace created for the
   adoption keeps the template, without resources, until removed.
6. Otherwise the user reviews and confirms; the apply imports, and from then on the resources are ordinary template
   resources. Imports belong to that one run only.

Verified end to end (November 2026): the portal-made Windows VM `testvm` adopted into workspace `test` with the
example VM module 3.1.0 from the registry: `2 to import, 0 to add, 0 to change, 0 to destroy`, applied, and the next
plan had no changes.

## Later phases

**Built (scan):** template `adopt` entries got `list`, `require`, `id_from`, `inputs`, `placeholder` (builder
fields too); `POST /workspaces/{name}/scans` queues a run with action `scan` (`runs.scan_template`,
`runs.candidates`, migration 005); the runner's `scan.go` runs `terraform query -json` with `include_resource`
(the attributes come as JSON, no generated HCL to parse), builds the candidates, and checks their fit with one plan
adopting all of them (module `c<i>` each, local throwaway state). The resource index stores resource IDs
(`state_version_resources.resource_id`) to mark candidates managed already, and adopting a managed ID is refused;
runs that take the state lock index it at their plan phase too, so older states get IDs without an apply. Live check against the test
subscription: both VMs found, every input filled as typed by hand before, IDs' casing normalized, both "fits",
25 s.

**Built (generate):** the builder's Generate from module (`registry/adopt.go`) writes the adopt entries from the
registry module's HCL (count conditions → show_if, var references → inputs, resource id references → id_from,
sensitive + ignore_changes → placeholder; azurerm VM list quirk from a small table); without a pattern an azurerm
ID is checked for Azure's shape; the runner drops list types the provider can't list and queries again. For the
example VM module it generates exactly the entries written by hand before.

Decisions for phases 2 and 3 (November 2026): a scan starts from a workspace (its credentials); adopted Windows VMs
get a generated placeholder for the admin password (the module ignores password changes); the fit check runs
automatically after a scan; adoption stays one resource at a time (pre-filled form), bulk later.


- **Phase 2, read and pre-fill**: a speculative read job (`plan -generate-config-out` in a scratch directory) and a
  template mapping from attributes to inputs (`"inputs": {"size": "size"}`), following references
  (`network_interface_ids[0]`). Needs an input kind that is hidden but sent to Terraform (adoption-only inputs).
  Sensitive attributes are never sent back.
- **Phase 3, scan and bulk adopt**: a `scan` run action (only generated `list` / data blocks, no backend, no state
  lock), a list of unmanaged resources per subscription, bulk adopt with 🟢/🟡/🔴, the fit report. Needs resource
  IDs in the resource index (today names and types only) and case-insensitive ID comparison.
- **Templates**: Linux and Windows VMs, Key Vault, storage account, SQL, PostgreSQL; data disks, extensions and
  managed identities as further optional inputs.

## Open questions

- Paging and limits of `list` blocks (`limit` defaults to 100) in large subscriptions.
- Linux VMs (SSH keys on import), VMs with data disks or extensions: not tested yet.
- Scan needs azurerm ≥ 5.7 while modules may pin `~> 4.0`; scan configurations pin their own provider.
