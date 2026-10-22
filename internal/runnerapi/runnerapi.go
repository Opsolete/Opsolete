// Package runnerapi defines the wire format between Form2Stack and its runners.
package runnerapi

const (
	Prefix      = "/api/runner/v1"
	StatePrefix = "/api/state/v1"      // Terraform http state backend: StatePrefix + "/<workspace id>"
	LeaseHeader = "X-Form2Stack-Lease" // fencing token returned by claim; required on every job update

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
	RunID        string         `json:"run_id"`
	WorkspaceID  string         `json:"workspace_id"`
	Phase        string         `json:"phase"`  // plan | apply
	Action       string         `json:"action"` // apply | destroy
	LeaseID      string         `json:"lease_id"`
	LeaseSeconds int            `json:"lease_seconds"`
	ModuleSource string         `json:"module_source"` // go-getter address; a leading "/" is relative to the server URL
	StateToken   string         `json:"state_token"`   // state backend password; valid while this run is planning or applying
	Vars         map[string]any `json:"vars"`
}

type FinishRequest struct {
	Status  string         `json:"status"` // succeeded | failed | canceled
	Message string         `json:"message,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
	// Plan is set by a plan phase that found changes; nil means nothing to do.
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
