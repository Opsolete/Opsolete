// Package runnerapi defines the wire format between Form2Stack and its runners.
package runnerapi

const (
	Prefix      = "/api/runner/v1"
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
	StateKey     string         `json:"state_key"`
	Vars         map[string]any `json:"vars"`
}

type FinishRequest struct {
	Status  string         `json:"status"` // succeeded | failed | canceled
	Message string         `json:"message,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
}
