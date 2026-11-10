// Package runnerapi defines the wire format between Opsolete and its runners.
package runnerapi

import (
	"encoding/json"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/templates"
)

const (
	Prefix      = "/api/runner/v1"
	StatePrefix = "/api/state/v1"    // Terraform http state backend: StatePrefix + "/<workspace id>"
	LeaseHeader = "X-Opsolete-Lease" // fencing token returned by claim; required on every job update

	// A claimed job is acknowledged (POST Prefix/jobs/<run>/ack with the lease, 204) before any other
	// call for it, which the server refuses (409) until then; one not acknowledged within 30 s of its
	// claim is queued again for any runner. 409 on the ack: the lease is gone, drop the job.

	// StatusCancel answers a log post or heartbeat when a user canceled the run: interrupt
	// Terraform (it may still save state), then finish as canceled. 409 means the lease is gone.
	StatusCancel = 202

	// Artifacts a plan phase uploads and the apply phase downloads: POST/GET Prefix/jobs/<run>/artifacts/<name>.
	ArtifactPlan     = "tfplan"
	ArtifactLockfile = "lockfile" // .terraform.lock.hcl, so the apply uses the providers the plan used

	// MaxJobSize bounds a claimed job's JSON (variables are ≤64 KiB each, but unlimited in number):
	// the server fails a bigger job at claim time instead of handing the runner a body it would truncate.
	MaxJobSize = 16 << 20
)

type ClaimRequest struct {
	Runner string `json:"runner"`
}

// Job is one phase of a run: "plan" saves a plan and reports what it will do; "apply" applies
// the plan saved by that run's plan phase (possibly on another runner, after confirmation).
type Job struct {
	RunID       string `json:"run_id"`
	WorkspaceID string `json:"workspace_id"`
	Phase       string `json:"phase"` // plan | apply
	// apply | destroy | refresh (refresh-only: the apply only updates state) | assess (drift check:
	// a refresh-only plan that is never applied, so its plan phase uploads nothing) | plan (speculative
	// plan of a pull request: never applied either)
	Action       string         `json:"action"`
	Replace      []string       `json:"replace,omitempty"` // resource addresses the plan replaces (-replace)
	Target       []string       `json:"target,omitempty"`  // addresses the plan is limited to (-target)
	Imports      []Import       `json:"imports,omitempty"` // existing resources the run adopts (import blocks)
	LeaseID      string         `json:"lease_id"`
	LeaseSeconds int            `json:"lease_seconds"`
	ModuleSource string         `json:"module_source"` // root configuration (a VCS commit archive): go-getter address; a leading "/" is relative to the server URL; "" = none
	Modules      []Module       `json:"modules,omitempty"`
	StateToken   string         `json:"state_token"` // state backend password; valid while this run is planning or applying
	Vars         map[string]any `json:"vars"`
	// Env is set for Terraform on top of the runner's own environment: the workspace's
	// environment variables, typically cloud credentials (ARM_CLIENT_SECRET, ...).
	Env map[string]string `json:"env,omitempty"`
	// Secrets are the values of sensitive variables and inputs: the runner masks them (internal/mask) in the log and outputs, as
	// a module may print them (local-exec, or a variable its module doesn't mark sensitive).
	Secrets []string `json:"secrets,omitempty"`
	// Currency of the cost estimate (ISO 4217, e.g. EUR); "" is Infracost's default, USD.
	Currency string `json:"currency,omitempty"`
	// Registry lets the run's Terraform fetch modules from the server's registry (a template's module calling
	// other registry modules): read-only, valid while a runner works on the run. nil: the registry is off.
	Registry *RegistryAccess `json:"registry,omitempty"`
	// Scan (action scan, plan phase only): what to look for. No state: nothing is written, nothing locked.
	Scan *Scan `json:"scan,omitempty"`
}

// RegistryAccess is a registry host and the token for it, as a Terraform CLI credentials block.
type RegistryAccess struct {
	Host  string `json:"host"` // e.g. opsolete.example.com, or with a port
	Token string `json:"token"`
}

// Scan finds existing resources the catalog's templates can take over (runner.scan).
type Scan struct {
	ResourceGroup string         `json:"resource_group,omitempty"` // narrows the search; "" the whole subscription
	Templates     []ScanTemplate `json:"templates"`
	Notes         []string       `json:"notes,omitempty"` // templates a scan can't use, and why, for the log
}

// ScanTemplate is a template a scan matches resources to. Entries, derived from its module's code
// (registry.Adoptable), say what to find and how it fills the form; Module names its module in Job.Modules,
// for the fit check.
type ScanTemplate struct {
	Template json.RawMessage `json:"template"` // templates.Template: its inputs, for show_if and the form's checks
	Module   string          `json:"module"`
	Entries  []ScanEntry     `json:"entries"`
	Notes    []string        `json:"notes,omitempty"` // what reading the module skipped, for the log
}

// ScanEntry is a resource of a template's module that can take over an existing one.
type ScanEntry struct {
	To      string            `json:"to"`                // its address in the module, e.g. azurerm_windows_virtual_machine.self[0]
	List    string            `json:"list"`              // the list resource type that finds it (terraform query)
	Require string            `json:"require,omitempty"` // only resources with this attribute set count
	IDFrom  string            `json:"id_from,omitempty"` // a related resource: its ID is the primary's attribute at this path
	ShowIf  *templates.ShowIf `json:"show_if,omitempty"` // it exists only for these values of an input, e.g. os_type windows
	Inputs  map[string]string `json:"inputs,omitempty"`  // input name → the attribute path that fills it
	// Placeholder: sensitive inputs it ignores (lifecycle ignore_changes), filled with a random value on import.
	Placeholder []string `json:"placeholder,omitempty"`
	// Of: an extra (optional or repeated) belonging to the resource at this address, found from it, not listed.
	Of string `json:"of,omitempty"`
	// Repeat: one instance per element of a JSON list input; To holds {key}.
	Repeat *Repeat `json:"repeat,omitempty"`
	// Lists: JSON list input name → the repeated block its elements fill.
	Lists map[string]BlockList `json:"lists,omitempty"`
}

// Repeat is a module instance per element of the JSON list input Input, keyed by the element's field Key.
type Repeat struct {
	Input  string            `json:"input"`
	Key    string            `json:"key"`
	Fields map[string]string `json:"fields"` // element field → the attribute path that fills it
}

// BlockList is a repeated nested block (a dynamic block): one JSON list element per block.
type BlockList struct {
	Block  string            `json:"block"`  // its path, e.g. security_rule
	Fields map[string]string `json:"fields"` // element field → the block's attribute
}

// Primary reports whether resources found for the entry are candidates (not related to another's, nor an extra).
func (e ScanEntry) Primary() bool { return e.IDFrom == "" && e.Of == "" }

// Module is a template added to the workspace: the runner adds it as module.<Name> next to the
// root configuration, and exposes each of its outputs as the root output <Name>__<output>.
type Module struct {
	Name   string         `json:"name"`
	Source string         `json:"source"` // like ModuleSource
	Inputs map[string]any `json:"inputs"`
}

// Import adopts an existing resource: Terraform imports ID into the address To (an import block).
type Import struct {
	To string `json:"to"` // e.g. module.vm_c8ad.azurerm_linux_virtual_machine.this
	ID string `json:"id"`
}

type FinishRequest struct {
	Status  string         `json:"status"` // succeeded | failed | canceled
	Message string         `json:"message,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
	// Plan is set by a plan phase that found changes; nil means nothing to do. For refresh and
	// assess runs it lists the drift: resources changed or deleted outside Terraform.
	Plan *Plan `json:"plan,omitempty"`
	// Resources indexes the workspace's state as the apply left it. null (not empty) means the
	// state couldn't be read, so the current version stays unindexed; [] means nothing is deployed.
	Resources []Resource `json:"resources"`
	// Candidates a scan found, by name. Inputs never hold sensitive values.
	Candidates []Candidate `json:"candidates,omitempty"`
	// Inventory: every resource the scan saw, templates or not (nil: it couldn't take stock).
	Inventory []Found `json:"inventory,omitempty"`
}

// Found is a resource a scan saw in the cloud: names only, no settings.
type Found struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"` // the cloud's, e.g. Microsoft.Network/virtualNetworks
	ResourceGroup string `json:"resource_group"`
	Location      string `json:"location,omitempty"`
}

// Candidate is an existing resource a scan found for a template, with its related ones: the form values and
// imports that take them over, and how a plan with them went (the fit check).
type Candidate struct {
	Template string            `json:"template"` // its id
	Name     string            `json:"name"`
	ID       string            `json:"id"`      // the resource's ID
	Inputs   map[string]string `json:"inputs"`  // form values by input name, as typed; sensitive ones left out
	Imports  []Import          `json:"imports"` // To: the address in the template's module
	// Placeholder: sensitive inputs to fill with a random value on import (ScanEntry.Placeholder).
	Placeholder []string `json:"placeholder,omitempty"`
	// Uses: other resources of the inventory its resources refer to (an OS disk, a public IP), not imported with it.
	Uses    []string    `json:"uses,omitempty"`
	Fit     string      `json:"fit"` // fits | differs | replaces | invalid (the form refuses its values) | unchecked
	Message string      `json:"message,omitempty"`
	Changes []FitChange `json:"changes,omitempty"` // differs / replaces: what the plan would do besides importing
}

// FitChange is one resource a fit check's plan changes: the action and the attributes, never their values.
type FitChange struct {
	Address    string   `json:"address"` // in the module, e.g. azurerm_windows_virtual_machine.self[0]
	Action     string   `json:"action"`  // create | update | delete | replace
	Attributes []string `json:"attributes,omitempty"`
}

// Plan is what a plan will do: counts and resource addresses, never values.
type Plan struct {
	Add     int      `json:"add"`
	Change  int      `json:"change"`
	Destroy int      `json:"destroy"`
	Import  int      `json:"import,omitempty"` // existing resources imported, with or without changes
	Changes []Change `json:"changes"`
	Cost    *Cost    `json:"cost,omitempty"` // nil: cost estimation is off or failed
}

// Cost is Infracost's estimate of a plan's monthly cost, without usage-based charges.
type Cost struct {
	Currency    string  `json:"currency"` // e.g. USD
	Prior       float64 `json:"prior"`    // before the plan
	Proposed    float64 `json:"proposed"` // after it
	Delta       float64 `json:"delta"`
	Priced      int     `json:"priced"`      // resources Infracost priced
	Unsupported int     `json:"unsupported"` // resources it can't price (left out of the totals)
}

type Change struct {
	Address string `json:"address"`
	Action  string `json:"action"` // create | update | delete | replace | import (imported without changes)
}

// Resource is one managed resource in a workspace's state: names and its ID, no other values.
type Resource struct {
	Address  string `json:"address"`  // e.g. module.net.azurerm_subnet.this[0]
	Type     string `json:"type"`     // e.g. azurerm_subnet
	Name     string `json:"name"`     // e.g. this
	Provider string `json:"provider"` // e.g. registry.terraform.io/hashicorp/azurerm
	Module   string `json:"module"`   // e.g. module.net; empty for the root module
	// ID is the resource's id attribute, the one value kept: a scan tells resources already managed by it.
	ID string `json:"id,omitempty"`
}

// Inventory lists the managed resources in a state (terraform show -json): names and IDs, no other values.
func Inventory(st *tfjson.State) []Resource {
	out := []Resource{}
	var walk func(m *tfjson.StateModule)
	walk = func(m *tfjson.StateModule) {
		for _, r := range m.Resources {
			if r.Mode == tfjson.ManagedResourceMode {
				id, _ := r.AttributeValues["id"].(string)
				out = append(out, Resource{Address: r.Address, Type: r.Type, Name: r.Name, Provider: r.ProviderName, Module: m.Address, ID: id})
			}
		}
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	if st.Values != nil && st.Values.RootModule != nil {
		walk(st.Values.RootModule)
	}
	return out
}
