package app

// Variables: environment variables (cloud credentials) and Terraform variables for a workspace's
// runs, set on the workspace (operate access) or in variable sets (admins, variablesets.go). Values
// are sealed with SECRET_KEY. Which ones a run gets (precedence, VCS-linked workspaces) is SQL's
// variable_set_rank.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// runVariables opens the variables of a workspace's run: environment variables for Terraform's
// environment, and Terraform variables.
func (a *App) runVariables(ctx context.Context, workspaceID uuid.UUID) (env map[string]string, vars map[string]any, secrets []string, err error) {
	list, err := a.Store.RunVariables(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load variables: %w", err)
	}
	env, vars = map[string]string{}, map[string]any{}
	for _, v := range list {
		val, err := a.Secrets.Open(v.Value, VariableAD(v))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("can't decrypt variable %s: SECRET_KEY missing or changed? Set the variable again", v.Key)
		}
		if v.Sensitive {
			secrets = append(secrets, val)
		}
		switch {
		case v.Category == "env":
			env[v.Key] = val
		case v.HCL:
			if vars[v.Key], err = hclValue(val); err != nil {
				return nil, nil, nil, fmt.Errorf("variable %s: not a valid HCL value: %w", v.Key, err)
			}
		default:
			vars[v.Key] = val
		}
	}
	return env, vars, secrets, nil
}

// workspaceMasker hides the workspace's sensitive variable and template input values (what its jobs
// get as runnerapi.Job.Secrets) in outputs the server reads from state files.
func (a *App) workspaceMasker(ctx context.Context, workspaceID uuid.UUID) (mask.Masker, error) {
	_, _, secrets, err := a.runVariables(ctx, workspaceID)
	if err != nil {
		return mask.Masker{}, err
	}
	mods, err := a.Store.WorkspaceTemplates(ctx, workspaceID)
	if err != nil {
		return mask.Masker{}, err
	}
	for _, m := range mods {
		_, sec, err := a.OpenInputs(m.Inputs, workspaceID, m.Name)
		if err != nil {
			return mask.Masker{}, err
		}
		secrets = append(append(secrets, sec...), a.plainSecrets(m)...)
	}
	return mask.New(secrets), nil
}

// An added template's module argument comes from, strongest first: the workspace's project's value
// for a per-environment input (withEnvironmentValues), the workspace's stored input (its form), the
// workspace's Terraform variables (own, then workspace sets, project sets, global sets: SQL's
// variable_set_rank) for variables the template doesn't ask, then the module's default.
//
// withWorkspaceVars adds to an added template's inputs the workspace's Terraform variables (vars, from
// the workspace and its variable sets) that its module declares, under its own inputs: as a root module
// gets them through tfvars, but only declared ones, since an unknown module argument fails the run.
// shortcut: registry modules only (their variables are stored); external sources such as git:: get
// none, as the server can't read their variables. Publish them to the registry (e.g. via VCS) to get
// them, as HCP Terraform's no-code modules must be. To upgrade: the runner has every module on disk
// after init (writeOutputs reads them there) and could add the declared ones itself, if the job also
// lists the inputs each template's form asks, which keep winning. Inputs t (the template at the module version it
// runs, generated inputs included; zero if it's no longer in the catalog) asks for are left alone, also when
// hidden or empty: its form decided those, and the module's default applies.
func (a *App) withWorkspaceVars(ctx context.Context, t templates.Template, source, ref string, in, vars map[string]any) (map[string]any, error) {
	if !a.RegistryEnabled || len(vars) == 0 {
		return in, nil
	}
	ns, name, sys, ok := a.Registry.ParseSource(source)
	if !ok {
		return in, nil
	}
	m, err := a.Store.GetModule(ctx, ns, name, sys, ref)
	if errors.Is(err, storage.ErrNotFound) { // the run fails at terraform init anyway
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("module variables: %w", err)
	}
	out := maps.Clone(in)
	if out == nil {
		out = map[string]any{}
	}
	for _, v := range m.Variables {
		if val, ok := vars[v.Name]; ok {
			asked := slices.ContainsFunc(t.Inputs, func(in templates.Input) bool { return in.Name == v.Name })
			if _, own := out[v.Name]; !own && !asked {
				out[v.Name] = val
			}
		}
	}
	return out, nil
}

var (
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	tfKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)
)

const maxVariableValue = 64 << 10

// reservedEnv: names that would let a variable run code on the runner or reconfigure Terraform
// itself. The runner's token claims every workspace's jobs, so a workspace must not take it over.
// shortcut: a denylist of the well-known vectors, not a sandbox; isolate runners per job if
// workspace operators aren't trusted with code execution on runners.
func reservedEnv(key string) bool {
	if key == "OPS_DEBUG" { // read from the job by the runner (full warnings), never its own settings
		return false
	}
	k := strings.ToUpper(key)
	switch k {
	case "PATH", "HOME", "SHELL", "TMPDIR", "BASH_ENV", "ENV", "NODE_OPTIONS", "PERL5OPT", "RUBYOPT",
		"SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND": // SHELLOPTS=xtrace + PS4='$(…)' runs code in bash
		return true
	}
	for _, p := range []string{"LD_", "DYLD_", "GIT_", "SSH_", "OPS_", "PYTHON"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	// TF_VAR_*: use a Terraform variable instead (the runner's tfexec.CleanEnv drops them from the environment).
	return strings.HasPrefix(k, "TF_")
}

// VariableInput is a variable to set, as typed; the key is trimmed.
type VariableInput struct {
	Category  string // env | terraform
	Key       string
	Value     string
	Sensitive bool
	HCL       bool // a Terraform variable whose value is an HCL expression ([1, 2], {a = "b"}, true), not a string
}

// sealVariable checks a variable of o (ValidationError on category, key or value) and seals its value.
func (a *App) sealVariable(o storage.VarOwner, in VariableInput) (storage.Variable, error) {
	cat, key, val := in.Category, strings.TrimSpace(in.Key), in.Value
	switch {
	case cat != "env" && cat != "terraform":
		return storage.Variable{}, invalid("category", "category must be env or terraform")
	case cat == "env" && (!envKeyRe.MatchString(key) || reservedEnv(key)):
		return storage.Variable{}, invalid("key", "an environment variable name is letters, digits and _; "+
			"PATH, HOME, LD_*, GIT_*, SSH_*, OPS_* and TF_* are reserved (use the terraform category for Terraform variables)")
	case cat == "terraform" && !tfKeyRe.MatchString(key):
		return storage.Variable{}, invalid("key", "a Terraform variable name is letters, digits, _ and -, starting with a letter or _")
	case len(val) > maxVariableValue:
		return storage.Variable{}, invalid("value", fmt.Sprintf("values are limited to %d KB", maxVariableValue>>10))
	case in.HCL && cat != "terraform":
		return storage.Variable{}, invalid("hcl", "only Terraform variables are HCL; environment variables are strings")
	}
	if in.HCL {
		if _, err := hclValue(val); err != nil {
			return storage.Variable{}, invalid("value", "not a valid HCL value: "+err.Error())
		}
	}
	v := storage.Variable{Category: cat, Key: key, Sensitive: in.Sensitive, HCL: in.HCL, Owner: o}
	var err error
	v.Value, err = a.Secrets.Seal(val, VariableAD(v))
	return v, err
}

// Variable is a variable as shown: a sensitive one's Value is always "".
type Variable struct {
	ID            uuid.UUID
	Category, Key string
	Value         string
	Sensitive     bool
	HCL           bool
}

// hclValue evaluates an HCL variable's value: a literal expression (no references, no functions), as JSON
// for the variables file Terraform reads.
func hclValue(src string) (any, error) {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "value", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}
	val, diags := expr.Value(nil) // nil context: a reference or function call is an error, not evaluated
	if diags.HasErrors() {
		return nil, errors.New(diags.Error())
	}
	if !val.IsWhollyKnown() {
		return nil, errors.New("the value isn't known")
	}
	b, err := ctyjson.Marshal(val, val.Type())
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(b, &out)
}

// variables lists o's variables with their values opened, except sensitive ones: those are never shown again.
func (a *App) variables(ctx context.Context, o storage.VarOwner) ([]Variable, error) {
	list, err := a.Store.Variables(ctx, o)
	if err != nil {
		return nil, err
	}
	out := make([]Variable, 0, len(list))
	for _, v := range list {
		vv := Variable{ID: v.ID, Category: v.Category, Key: v.Key, Sensitive: v.Sensitive, HCL: v.HCL}
		if !v.Sensitive {
			if vv.Value, err = a.Secrets.Open(v.Value, VariableAD(v)); err != nil {
				vv.Value = "(can't decrypt: SECRET_KEY changed? Set it again)"
			}
		}
		out = append(out, vv)
	}
	return out, nil
}

// SetVariables is a variable set with its variables.
type SetVariables struct {
	storage.VariableSet
	Vars []Variable
}

// WorkspaceVariables is a workspace's Variables page: its own, and each set that applies to it.
type WorkspaceVariables struct {
	Own  []Variable
	Sets []SetVariables
}

// WorkspaceVariables lists a workspace's variables and the sets that apply to it; read access.
func (a *App) WorkspaceVariables(ctx context.Context, actor Actor, workspace string) (WorkspaceVariables, error) {
	var out WorkspaceVariables
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelRead)
	if err != nil {
		return out, err
	}
	if out.Own, err = a.variables(ctx, storage.VarOwner{WorkspaceID: ws.ID}); err != nil {
		return out, err
	}
	sets, err := a.Store.WorkspaceVariableSets(ctx, ws.ID)
	if err != nil {
		return out, err
	}
	for _, set := range sets {
		vars, err := a.variables(ctx, storage.VarOwner{SetID: set.ID})
		if err != nil {
			return out, err
		}
		out.Sets = append(out.Sets, SetVariables{set, vars})
	}
	return out, nil
}

// SetWorkspaceVariable adds or replaces (by category and key) a workspace's variable; operate access.
func (a *App) SetWorkspaceVariable(ctx context.Context, actor Actor, workspace string, in VariableInput) error {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return err
	}
	o := storage.VarOwner{WorkspaceID: ws.ID}
	v, err := a.sealVariable(o, in)
	if err != nil {
		return err
	}
	if err := a.Store.SetVariable(ctx, o, v); err != nil {
		return err
	}
	a.Log.Info("variable set", "workspace", ws.Name, "category", v.Category, "key", v.Key, "user", actor.Email)
	return nil
}

// DeleteWorkspaceVariable deletes a workspace's variable; an unknown id deletes nothing. Operate access.
func (a *App) DeleteWorkspaceVariable(ctx context.Context, actor Actor, workspace string, id uuid.UUID) error {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return err
	}
	if err := a.Store.DeleteVariable(ctx, storage.VarOwner{WorkspaceID: ws.ID}, id); err != nil {
		return err
	}
	a.Log.Info("variable deleted", "workspace", ws.Name, "id", id, "user", actor.Email)
	return nil
}
