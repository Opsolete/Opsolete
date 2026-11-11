package app

// Per-environment inputs: a template input marked per_environment takes, at every run, the value the
// workspace's project sets for it ("Environment values" on the project), over the workspace's own
// (withEnvironmentValues); with keep_own, only in workspaces without an own value (Locked). Projects are the environments. Admins and the groups that may create
// workspaces in a project (operate on it) set its values; whoever sees the project reads them.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// EnvironmentInput is a per-environment input of a template and the project's value for it.
type EnvironmentInput struct {
	templates.Input
	Set       bool   // the project has a value
	Value     string // its canonical form ("7", "true")
	KeepOwn   bool   // workspaces with their own value keep it
	Text      string // as the form says it (an option's label, Yes / No)
	UpdatedAt time.Time
	UpdatedBy string // an email, "" if unknown
}

// EnvironmentTemplate is a template with per-environment inputs, in the form's order.
type EnvironmentTemplate struct {
	Template templates.Template
	Inputs   []EnvironmentInput
}

// EnvironmentValues is a project's Environment values page.
type EnvironmentValues struct {
	Project   storage.Project
	CanEdit   bool
	Templates []EnvironmentTemplate // the catalog's templates with per-environment inputs, by name
}

// EnvironmentValues lists the project's values for every template of the catalog with
// per-environment inputs, also inputs without one. Anyone who sees the project; ErrNotFound.
func (a *App) EnvironmentValues(ctx context.Context, actor Actor, project string) (EnvironmentValues, error) {
	var out EnvironmentValues
	var err error
	if out.Project, err = a.Project(ctx, actor, project); err != nil {
		return out, err
	}
	if out.CanEdit, err = a.mayEditEnvironment(ctx, actor, out.Project.ID); err != nil {
		return out, err
	}
	list, err := a.Store.EnvironmentValues(ctx, out.Project.ID)
	if err != nil {
		return out, err
	}
	stored := map[[2]string]storage.EnvironmentValue{}
	for _, v := range list {
		stored[[2]string{v.TemplateID, v.Input}] = v
	}
	for _, t := range a.Templates.List() {
		if t, err = a.Resolve(ctx, t); err != nil {
			return out, err
		}
		et := EnvironmentTemplate{Template: t}
		for _, in := range t.Inputs {
			if !in.PerEnvironment {
				continue
			}
			ei := EnvironmentInput{Input: in}
			if v, ok := stored[[2]string{t.ID, in.Name}]; ok {
				ei.Set, ei.Value, ei.Text, ei.KeepOwn, ei.UpdatedAt, ei.UpdatedBy = true, v.Value, ValueText(in, v.Value), v.KeepOwn, v.UpdatedAt, v.UpdatedBy
			}
			et.Inputs = append(et.Inputs, ei)
		}
		if len(et.Inputs) > 0 {
			out.Templates = append(out.Templates, et)
		}
	}
	return out, nil
}

// mayEditEnvironment: actor may set the project's values. The rule of creating workspaces in it
// (CreatableProjects): admins, and groups with operate on the project.
func (a *App) mayEditEnvironment(ctx context.Context, actor Actor, projectID uuid.UUID) (bool, error) {
	list, err := a.CreatableProjects(ctx, actor)
	return slices.ContainsFunc(list, func(p storage.Project) bool { return p.ID == projectID }), err
}

// environmentInput loads what setting or clearing a project's value needs: the project (ErrNotFound
// unless actor sees it, ErrForbidden unless actor may edit it) and the template's per-environment
// input (ErrNotFound otherwise).
func (a *App) environmentInput(ctx context.Context, actor Actor, project, templateID, input string) (storage.Project, templates.Input, error) {
	p, err := a.Project(ctx, actor, project)
	if err != nil {
		return p, templates.Input{}, err
	}
	if ok, err := a.mayEditEnvironment(ctx, actor, p.ID); err != nil || !ok {
		return p, templates.Input{}, cmp.Or(err, ErrForbidden)
	}
	t, err := a.Template(ctx, templateID)
	if err != nil {
		return p, templates.Input{}, err
	}
	i := slices.IndexFunc(t.Inputs, func(in templates.Input) bool { return in.Name == input && in.PerEnvironment })
	if i < 0 {
		return p, templates.Input{}, ErrNotFound
	}
	return p, t.Inputs[i], nil
}

// SetEnvironmentValue sets the project's value for a template's per-environment input, checked by
// the input's own rules (type, options, pattern; required doesn't apply: clear it instead). keepOwn: workspaces with
// their own value keep it (an imported VM's subnet); the others take this one.
// ValidationError (field "value"); see environmentInput for the others.
func (a *App) SetEnvironmentValue(ctx context.Context, actor Actor, project, templateID, input, value string, keepOwn bool) error {
	p, in, err := a.environmentInput(ctx, actor, project, templateID, input)
	if err != nil {
		return err
	}
	v, err := environmentValue(in, value)
	if err != nil {
		return invalid("value", err.Error())
	}
	if v == "" {
		return invalid("value", "Enter a value, or clear it.")
	}
	if err := a.Store.SetEnvironmentValue(ctx, p.ID, templateID, in.Name, v, keepOwn, actor.UserID); err != nil {
		return err
	}
	a.Log.Info("environment value set", "project", p.Name, "template", templateID, "input", in.Name, "keep_own", keepOwn, "user", actor.Email)
	return nil
}

// ClearEnvironmentValue removes the project's value for a template's per-environment input: its
// workspaces use their own again. See environmentInput for the errors.
func (a *App) ClearEnvironmentValue(ctx context.Context, actor Actor, project, templateID, input string) error {
	p, in, err := a.environmentInput(ctx, actor, project, templateID, input)
	if err != nil {
		return err
	}
	if err := a.Store.ClearEnvironmentValue(ctx, p.ID, templateID, in.Name); err != nil {
		return err
	}
	a.Log.Info("environment value cleared", "project", p.Name, "template", templateID, "input", in.Name, "user", actor.Email)
	return nil
}

// environmentValue checks raw (as typed in a form) by in's rules and returns its canonical form, ""
// for none. The input stands alone: no show_if, not required.
func environmentValue(in templates.Input, raw string) (string, error) {
	in.ShowIf, in.Required = nil, false
	vals, errs := templates.Template{Inputs: []templates.Input{in}}.Validate(map[string]string{in.Name: raw})
	if e := errs[in.Name]; e != "" {
		return "", errors.New(e)
	}
	v, ok := vals[in.Name]
	if !ok {
		return "", nil
	}
	return templates.FormatValue(v), nil
}

// ValueText is how the form says an input's canonical value v: an option's label, Yes / No.
func ValueText(in templates.Input, v string) string {
	if in.Type == "bool" {
		return map[bool]string{true: "Yes", false: "No"}[v == "true"]
	}
	for _, o := range in.Options {
		if o.Value == v {
			return o.Text()
		}
	}
	return v
}

// EnvironmentMap is a project's values by template id, then input name.
type EnvironmentMap map[string]map[string]storage.EnvironmentValue

func environmentMap(list []storage.EnvironmentValue, err error) (EnvironmentMap, error) {
	m := EnvironmentMap{}
	for _, v := range list {
		if m[v.TemplateID] == nil {
			m[v.TemplateID] = map[string]storage.EnvironmentValue{}
		}
		m[v.TemplateID][v.Input] = v
	}
	return m, err
}

// Locked returns the values (canonical, by input name) env sets for t's per-environment inputs in a workspace whose
// own values are own (stored, or found by a scan when importing): the inputs a form shows read-only, a save ignores
// and runs take from the project. A keep_own value doesn't lock an input whose own value differs: that one is kept.
func (env EnvironmentMap) Locked(t templates.Template, own map[string]any) map[string]string {
	out := map[string]string{}
	for _, in := range t.Inputs {
		v, ok := env[t.ID][in.Name]
		if !ok || !in.PerEnvironment {
			continue
		}
		if o, has := own[in.Name]; v.KeepOwn && has && templates.FormatValue(o) != v.Value {
			continue
		}
		out[in.Name] = v.Value
	}
	return out
}

// Describe is t.Describe(stored) with the values env (project's) sets for t's per-environment
// inputs in their place: what the workspace runs.
func (env EnvironmentMap) Describe(t templates.Template, stored map[string]any, project string) []templates.StoredValue {
	locked := env.Locked(t, stored)
	stored = maps.Clone(stored)
	for name := range locked {
		stored[name] = "" // listed in the form's order, also without a value of its own
	}
	out := t.Describe(stored)
	for i, sv := range out {
		if v, ok := locked[sv.Name]; ok {
			in := t.Inputs[slices.IndexFunc(t.Inputs, func(in templates.Input) bool { return in.Name == sv.Name })]
			out[i].Text, out[i].Project = ValueText(in, v), project
		}
	}
	return out
}

// lockInputs prepares a template form's save for the inputs the project sets (locked, from
// Locked): raw gets the project's values, so validation and show_if see what runs. Call keepOwn
// after validating.
func lockInputs(raw, locked map[string]string) map[string]string {
	raw = maps.Clone(raw)
	maps.Copy(raw, locked)
	return raw
}

// keepOwn ignores what the form posted for the locked inputs: each keeps the workspace's own stored
// value (old), none if it had none.
func keepOwn(vals map[string]any, errs map[string]string, locked map[string]string, old map[string]any) {
	for name := range locked {
		delete(errs, name)
		if _, shown := vals[name]; !shown {
			continue
		}
		if v, ok := old[name]; ok {
			vals[name] = v
		} else {
			delete(vals, name)
		}
	}
}

// ProjectEnvironment is the project's values, for the forms of its workspaces.
func (a *App) ProjectEnvironment(ctx context.Context, projectID uuid.UUID) (EnvironmentMap, error) {
	return environmentMap(a.Store.EnvironmentValues(ctx, projectID))
}

// withEnvironmentValues puts into an added template's inputs (in: stored and opened) the values the
// workspace's project sets for its per-environment inputs (env, the project's values at claim time),
// over the workspace's own: see withWorkspaceVars for the whole precedence. An input the form would
// hide (show_if, evaluated with the project's values) isn't set, as the form wouldn't send it. A
// template no longer in the catalog has no known per-environment inputs: its stored inputs apply.
// A project value the template's current rules reject fails the job: deploying the workspace's own
// value instead in, say, production would be worse than not running.
func (a *App) withEnvironmentValues(ctx context.Context, templateID string, in map[string]any, env EnvironmentMap) (map[string]any, error) {
	if len(env[templateID]) == 0 {
		return in, nil
	}
	t, err := a.Template(ctx, templateID)
	if errors.Is(err, ErrNotFound) {
		return in, nil
	}
	if err != nil {
		return in, err
	}
	locked := env.Locked(t, in)
	if len(locked) == 0 {
		return in, nil
	}
	raw := map[string]string{} // the form's view, for show_if
	for k, v := range in {
		raw[k] = templates.FormatValue(v)
	}
	maps.Copy(raw, locked)
	out := maps.Clone(in)
	if out == nil {
		out = map[string]any{}
	}
	for _, input := range t.Inputs {
		v, ok := locked[input.Name]
		if !ok || !t.Match(input.ShowIf, raw) {
			continue
		}
		input.ShowIf, input.Required = nil, false
		vals, errs := templates.Template{Inputs: []templates.Input{input}}.Validate(map[string]string{input.Name: v})
		if errs[input.Name] != "" {
			return in, fmt.Errorf("the project's environment value for input %s of template %s is no longer valid (%s): set it again", input.Name, t.Name, errs[input.Name])
		}
		out[input.Name] = vals[input.Name]
	}
	return out, nil
}
