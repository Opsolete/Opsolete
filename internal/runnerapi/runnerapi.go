// Package runnerapi defines the wire format between Opsolete and its runners.
package runnerapi

import tfjson "github.com/hashicorp/terraform-json"

const (
	Prefix      = "/api/runner/v1"
	StatePrefix = "/api/state/v1"    // Terraform http state backend: StatePrefix + "/<workspace id>"
	LeaseHeader = "X-Opsolete-Lease" // fencing token returned by claim; required on every job update

	// StatusCancel answers a log post or heartbeat when a user canceled the run: interrupt
	// Terraform (it may still save state), then finish as canceled. 409 means the lease is gone.
	StatusCancel = 202

	// Artifacts a plan phase uploads and the apply phase downloads: POST/GET Prefix/jobs/<run>/artifacts/<name>.
	ArtifactPlan     = "tfplan"
	ArtifactLockfile = "lockfile" // .terraform.lock.hcl, so the apply uses the providers the plan used
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
	LeaseID      string         `json:"lease_id"`
	LeaseSeconds int            `json:"lease_seconds"`
	ModuleSource string         `json:"module_source"` // go-getter address; a leading "/" is relative to the server URL; "" = none
	Modules      []Module       `json:"modules,omitempty"`
	StateToken   string         `json:"state_token"` // state backend password; valid while this run is planning or applying
	Vars         map[string]any `json:"vars"`
	// Env is set for Terraform on top of the runner's own environment: the workspace's
	// environment variables, typically cloud credentials (ARM_CLIENT_SECRET, ...).
	Env map[string]string `json:"env,omitempty"`
}

// Module is a template added to the workspace: the runner adds it as module.<Name> next to the
// root configuration, and exposes each of its outputs as the root output <Name>__<output>.
type Module struct {
	Name   string         `json:"name"`
	Source string         `json:"source"` // like ModuleSource
	Inputs map[string]any `json:"inputs"`
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
}

// Plan is what a plan will do: counts and resource addresses, never values.
type Plan struct {
	Add     int      `json:"add"`
	Change  int      `json:"change"`
	Destroy int      `json:"destroy"`
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
	Action  string `json:"action"` // create | update | delete | replace
}

// Resource is one managed resource in a workspace's state; never its attribute values.
type Resource struct {
	Address  string `json:"address"`  // e.g. module.net.azurerm_subnet.this[0]
	Type     string `json:"type"`     // e.g. azurerm_subnet
	Name     string `json:"name"`     // e.g. this
	Provider string `json:"provider"` // e.g. registry.terraform.io/hashicorp/azurerm
	Module   string `json:"module"`   // e.g. module.net; empty for the root module
}

// Inventory lists the managed resources in a state (terraform show -json): names only, never values.
func Inventory(st *tfjson.State) []Resource {
	out := []Resource{}
	var walk func(m *tfjson.StateModule)
	walk = func(m *tfjson.StateModule) {
		for _, r := range m.Resources {
			if r.Mode == tfjson.ManagedResourceMode {
				out = append(out, Resource{Address: r.Address, Type: r.Type, Name: r.Name, Provider: r.ProviderName, Module: m.Address})
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
