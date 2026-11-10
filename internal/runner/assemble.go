package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

const (
	planFile    = "opsolete.tfplan"
	lockFile    = ".terraform.lock.hcl"
	modulesFile = "opsolete_templates.tf.json" // module blocks of the workspace's added templates
	outputsFile = "opsolete_outputs.tf.json"   // their outputs, as root outputs
	importsFile = "opsolete_imports.tf.json"   // import blocks of the existing resources the run adopts
)

// dropRemoved finishes removing templates after the destroy limited to them (-target=module.<name>):
// Terraform keeps all outputs on a targeted destroy, so it plans the configuration without those
// templates (their resources are gone, so their providers aren't needed anymore) and applies that
// plan if it only changes outputs. -refresh=false: no API calls, and drift can't sneak in.
func dropRemoved(ctx context.Context, tf *tfProc, run func(...string) (bool, error), dir string, job runnerapi.Job, w io.Writer) error {
	left := slices.DeleteFunc(slices.Clone(job.Modules), func(m runnerapi.Module) bool { return slices.Contains(job.Target, "module."+m.Name) })
	for _, f := range []string{modulesFile, outputsFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if len(left) > 0 { // an empty module block list is invalid
		if err := writeModules(dir, left); err != nil {
			return err
		}
	}
	if err := writeOutputs(dir, left); err != nil {
		return err
	}
	const cleanupPlan = "opsolete-cleanup.tfplan"
	fmt.Fprintln(w, "\n==> terraform plan -refresh=false (without the removed templates, to drop their outputs)")
	changes, err := run("plan", "-input=false", "-detailed-exitcode", "-out="+cleanupPlan, "-refresh=false")
	if err != nil || !changes {
		return err
	}
	var p tfjson.Plan
	if err := tf.json(ctx, &p, "show", "-json", "-no-color", cleanupPlan); err != nil {
		return err
	}
	for _, rc := range p.ResourceChanges {
		if rc.Change != nil && !rc.Change.Actions.NoOp() && !rc.Change.Actions.Read() {
			return fmt.Errorf("the plan also changes %s: not applied", rc.Address)
		}
	}
	fmt.Fprintln(w, "\n==> terraform apply (outputs only)")
	_, err = run("apply", "-auto-approve", "-input=false", cleanupPlan)
	return err
}

// templateEscape keeps an input a value: strings in .tf.json are templates, so "${...}" in a form
// value would otherwise be evaluated (e.g. reading the runner's files or credentials).
var templateEscape = strings.NewReplacer("${", "$${", "%{", "%%{")

// escapeValue applies templateEscape to every string in v, nested ones and object keys included
// (HCL JSON evaluates property names as templates too).
func escapeValue(v any) any {
	switch v := v.(type) {
	case string:
		return templateEscape.Replace(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = escapeValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[templateEscape.Replace(k)] = escapeValue(e)
		}
		return out
	}
	return v
}

// writeModules writes a module block per added template, inputs as literal arguments.
func writeModules(dir string, mods []runnerapi.Module) error {
	blocks := map[string]map[string]any{}
	for _, m := range mods {
		b := map[string]any{}
		for k, v := range m.Inputs {
			b[k] = escapeValue(v)
		}
		b["source"] = m.Source // last: an input can't replace it
		blocks[m.Name] = b
	}
	j, err := json.Marshal(map[string]any{"module": blocks})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, modulesFile), j, 0o600)
}

// writeImports writes an import block per adopted resource. The ID is escaped like an input: it's a string
// in .tf.json, so "${...}" in it would be evaluated.
func writeImports(dir string, imports []runnerapi.Import) error {
	blocks := make([]map[string]string, len(imports))
	for i, im := range imports {
		blocks[i] = map[string]string{"to": im.To, "id": templateEscape.Replace(im.ID)}
	}
	j, err := json.Marshal(map[string]any{"import": blocks})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, importsFile), j, 0o600)
}

// writeOutputs exposes every output of the added templates as root output <name>__<output>, with
// the module's sensitivity (Terraform requires it), so the run reports them like root outputs.
func writeOutputs(dir string, mods []runnerapi.Module) error {
	var manifest struct{ Modules []struct{ Key, Dir string } }
	b, err := os.ReadFile(filepath.Join(dir, ".terraform", "modules", "modules.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return err
	}
	outs := map[string]any{}
	for _, m := range manifest.Modules {
		if !slices.ContainsFunc(mods, func(a runnerapi.Module) bool { return a.Name == m.Key }) {
			continue
		}
		mod, diags := tfconfig.LoadModule(filepath.Join(dir, m.Dir))
		if diags.HasErrors() {
			return diags.Err()
		}
		for name, o := range mod.Outputs {
			outs[m.Key+"__"+name] = map[string]any{"value": "${module." + m.Key + "." + name + "}", "sensitive": o.Sensitive}
		}
	}
	if len(outs) == 0 {
		return nil
	}
	j, err := json.Marshal(map[string]any{"output": outs})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, outputsFile), j, 0o600)
}

// backendArgs points Terraform's http backend at the workspace's state on the server. Its
// credentials come from TF_HTTP_USERNAME/TF_HTTP_PASSWORD (see terraform), so the token stays off the
// command line, .terraform/ and the saved plan. It only works while this run is planning or applying
// (module code can read it from the env): it reaches this workspace's state and, read-only, the states
// shared with it.
func (r *Runner) backendArgs(job runnerapi.Job) []string {
	addr := r.ServerURL.JoinPath(runnerapi.StatePrefix, job.WorkspaceID).String()
	return []string{"-backend-config=address=" + addr, "-backend-config=lock_address=" + addr, "-backend-config=unlock_address=" + addr}
}
