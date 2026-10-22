// Package runnerapi defines the wire format between Form2Stack and its runners.
package runnerapi

const (
	Prefix      = "/api/runner/v1"
	StatePrefix = "/api/state/v1"      // Terraform http state backend: StatePrefix + "/<deployment id>"
	LeaseHeader = "X-Form2Stack-Lease" // fencing token returned by claim; required on every job update
)

type ClaimRequest struct {
	Runner string `json:"runner"`
}

// Job is one deployment to execute.
type Job struct {
	DeploymentID string         `json:"deployment_id"`
	LeaseID      string         `json:"lease_id"`
	LeaseSeconds int            `json:"lease_seconds"`
	ModuleSource string         `json:"module_source"` // go-getter address; a leading "/" is relative to the server URL
	StateToken   string         `json:"state_token"`   // state backend password; valid only while this lease is
	Action       string         `json:"action"`        // apply | destroy
	Vars         map[string]any `json:"vars"`
}

type FinishRequest struct {
	Status  string         `json:"status"` // succeeded | failed | canceled
	Message string         `json:"message,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
	// Resources indexes the workspace's state as the run left it. null (not empty) means the
	// state couldn't be read, so the current version stays unindexed; [] means nothing is deployed.
	Resources []Resource `json:"resources"`
}

// Resource is one managed resource in a deployment's state; never its attribute values.
type Resource struct {
	Address  string `json:"address"`  // e.g. module.net.azurerm_subnet.this[0]
	Type     string `json:"type"`     // e.g. azurerm_subnet
	Name     string `json:"name"`     // e.g. this
	Provider string `json:"provider"` // e.g. registry.terraform.io/hashicorp/azurerm
	Module   string `json:"module"`   // e.g. module.net; empty for the root module
}
