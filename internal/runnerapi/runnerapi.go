// Package runnerapi defines the wire format between Opsolete and its runners.
package runnerapi

import (
	tfjson "github.com/hashicorp/terraform-json"
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
}

// RegistryAccess is a registry host and the token for it, as a Terraform CLI credentials block.
type RegistryAccess struct {
	Host  string `json:"host"` // e.g. opsolete.example.com, or with a port
	Token string `json:"token"`
}

// Module is a template added to the workspace: the runner adds it as module.<Name> next to the
// root configuration, and exposes each of its outputs as the root output <Name>__<output>.
type Module struct {
	Name   string         `json:"name"`
	Source string         `json:"source"` // like ModuleSource
	Inputs map[string]any `json:"inputs"`
}

// Import is an existing resource a plan imports (an import block, in a template's module): ID into the address To.
type Import struct {
	To string `json:"to"` // e.g. module.vm_c8ad.module.windows[0].azurerm_windows_virtual_machine.main
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
	// Found: a plan that imports, per template module (Module.Name), the inputs that alone set an imported
	// attribute, with that attribute's real value (list inputs: whole, their rows' fields filled in). Never
	// sensitive values. The form's "Use found values".
	Found map[string]map[string]any `json:"found,omitempty"`
}

// Plan is what a plan will do: counts, resource addresses and the IDs it imports, never other values.
type Plan struct {
	Add     int      `json:"add"`
	Change  int      `json:"change"`
	Destroy int      `json:"destroy"`
	Import  int      `json:"import,omitempty"`  // existing resources imported, with or without changes
	Imports []Import `json:"imports,omitempty"` // which: a state may hold each resource once (the import guard)
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
	// ID is the resource's id attribute, the one value kept: an import of a resource a state manages is refused.
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
