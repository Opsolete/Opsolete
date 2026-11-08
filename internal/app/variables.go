package app

// Variables of runs. Their CRUD (workspace variables, variable sets) follows in phase 2 step 2.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
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
		val, err := vcs.Open(a.SecretKey, v.Value, vcs.VariableAD(v))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("can't decrypt variable %s: SECRET_KEY missing or changed? Set the variable again", v.Key)
		}
		if v.Sensitive {
			secrets = append(secrets, val)
		}
		if v.Category == "env" {
			env[v.Key] = val
		} else {
			vars[v.Key] = val
		}
	}
	return env, vars, secrets, nil
}

// withWorkspaceVars adds to an added template's inputs the workspace's Terraform variables (vars, from
// the workspace and its variable sets) that its module declares, under its own inputs: as a root module
// gets them through tfvars, but only declared ones, since an unknown module argument fails the run.
// shortcut: registry modules only (their variables are stored); external sources such as git:: get
// none, as the server can't read their variables. Publish them to the registry (e.g. via VCS) to get
// them, as HCP Terraform's no-code modules must be. To upgrade: the runner has every module on disk
// after init (writeOutputs reads them there) and could add the declared ones itself, if the job also
// lists the inputs each template's form asks, which keep winning. Inputs the template (by id) asks for are left alone, also when
// hidden or empty: its form decided those, and the module's default applies.
func (a *App) withWorkspaceVars(ctx context.Context, templateID, source, ref string, in, vars map[string]any) (map[string]any, error) {
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
	t, _ := a.Templates.Get(templateID) // a deleted template: only its stored inputs are known
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
