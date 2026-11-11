package app

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Template returns template id with its inputs resolved (Resolve); ErrNotFound if the catalog lacks it.
func (a *App) Template(ctx context.Context, id string) (templates.Template, error) {
	t, ok := a.Templates.Get(id)
	if !ok {
		return t, ErrNotFound
	}
	return a.Resolve(ctx, t)
}

// Resolve returns t with its inputs resolved from the variables of the registry module version it
// pins (registry.Resolve), so the form can't drift from the module's variables.tf.
func (a *App) Resolve(ctx context.Context, t templates.Template) (templates.Template, error) {
	if !a.RegistryEnabled {
		return t, nil
	}
	// shortcut: git:: and other external sources keep their JSON inputs, since the server doesn't fetch modules. Publish them to the registry (e.g. via VCS) to get generated inputs.
	ns, name, sys, isReg := a.Registry.ParseSource(t.Module.Source)
	if !isReg {
		return t, nil
	}
	m, err := a.Store.GetModule(ctx, ns, name, sys, t.Module.Ref)
	if errors.Is(err, storage.ErrNotFound) { // saveTemplate refuses this; a hand-written JSON fails at terraform init
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.Inputs = registry.Resolve(t.Inputs, m.Variables)
	return t, nil
}

// Templates added to a workspace, any number: each is the module block module.<name> next to the
// workspace's own configuration, planned and applied together in its one state.

// TemplateForm is what the form to add a template to a workspace, or to configure an added one, needs.
type TemplateForm struct {
	Workspace storage.Workspace
	Template  templates.Template
	Current   *storage.WorkspaceTemplate // configuring: the added template; nil when adding
	Added     []storage.WorkspaceTemplate
}

// TemplateForm loads the form to add templateID (module "") to the workspace, or to configure its
// added template module (templateID is then the one it was added from). Needs operate.
func (a *App) TemplateForm(ctx context.Context, actor Actor, workspace, module, templateID string) (TemplateForm, error) {
	var f TemplateForm
	var err error
	if f.Workspace, _, err = a.Workspace(ctx, actor, workspace, LevelOperate); err != nil {
		return f, err
	}
	if f.Added, err = a.Store.WorkspaceTemplates(ctx, f.Workspace.ID); err != nil {
		return f, err
	}
	if module != "" {
		i := slices.IndexFunc(f.Added, func(t storage.WorkspaceTemplate) bool { return t.Name == module })
		if i < 0 {
			return f, ErrNotFound
		}
		f.Current, templateID = &f.Added[i], f.Added[i].TemplateID
	}
	f.Template, err = a.Template(ctx, templateID)
	return f, err
}

// SaveTemplate adds templateID to the workspace (module "") or saves the added template module's
// inputs (raw: as typed, by input name) and the template's current module version, and queues the
// plan that applies it. found (nil: none) is what an import plan found that the form took (FoundValues): those
// values count as the workspace's own, so a project's keep-own value doesn't replace them. It returns the module
// name. ValidationError: the inputs (Fields by input name).
func (a *App) SaveTemplate(ctx context.Context, actor Actor, workspace, module, templateID string, raw map[string]string, found map[string]any) (string, storage.Run, error) {
	f, err := a.TemplateForm(ctx, actor, workspace, module, templateID)
	if err != nil {
		return module, storage.Run{}, err
	}
	t, adding, old := f.Template, f.Current == nil, map[string]any{}
	if adding {
		module = defaultModule(t, raw, f.Added)
	} else {
		old = f.Current.Inputs
	}
	env, err := a.ProjectEnvironment(ctx, f.Workspace.ProjectID)
	if err != nil {
		return module, storage.Run{}, err
	}
	own := maps.Clone(old)
	maps.Copy(own, found)
	locked := env.Locked(t, own)
	raw = lockInputs(raw, locked)
	vals, errs := validate(t, raw)
	if err := a.SealInputs(t, raw, vals, old, errs, f.Workspace.ID, module); err != nil {
		return module, storage.Run{}, err
	}
	keepOwn(vals, errs, locked, old)
	if len(errs) > 0 {
		return module, storage.Run{}, &ValidationError{Fields: errs}
	}
	run, err := a.Store.SetWorkspaceTemplate(ctx, f.Workspace.ID, storage.WorkspaceTemplate{Name: module, TemplateID: t.ID,
		ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals}, adding, actor.UserID)
	if errors.Is(err, storage.ErrExists) {
		return module, run, &ValidationError{Message: "this workspace already has a template named " + module + " (added meanwhile), try again"}
	}
	if err != nil {
		return module, run, err
	}
	a.Log.Info("workspace template saved", "workspace", f.Workspace.Name, "module", module, "template", t.ID, "added", adding,
		"ref", t.Module.Ref, "run", run.ID, "found", found != nil, "user", actor.Email)
	return module, run, nil
}

// ImportInput is the input with which a template's module imports what exists instead of creating it (a bool);
// an applied import switches it off again (storage.FinishJob).
const ImportInput = storage.ImportInput

// validate is t.Validate, except while the module imports (ImportInput on): then the plan finds the settings, so
// only the key input is required, and a required input left empty is null (the module call needs the argument).
func validate(t templates.Template, raw map[string]string) (map[string]any, map[string]string) {
	i := slices.IndexFunc(t.Inputs, func(in templates.Input) bool { return in.Name == ImportInput && in.Type == "bool" })
	if i < 0 || raw[ImportInput] != "on" && raw[ImportInput] != "true" { // posted only when shown
		return t.Validate(raw)
	}
	t.Inputs = slices.Clone(t.Inputs)
	var optional []templates.Input
	for i, in := range t.Inputs {
		if in.Required && !in.Key && !in.Sensitive {
			t.Inputs[i].Required = false
			optional = append(optional, t.Inputs[i])
		}
	}
	vals, errs := t.Validate(raw)
	for _, in := range optional {
		if _, set := vals[in.Name]; !set && errs[in.Name] == "" && t.Match(in.ShowIf, raw) {
			vals[in.Name] = nil
		}
	}
	return vals, errs
}

// FoundValues is what import plan runID of the workspace found for its template module (added from t, now or
// before the run's restore removed it): the real values of t's inputs that alone set an imported attribute
// (runnerapi.FinishRequest.Found), typed as stored, sensitive inputs never. Needs operate. ErrNotFound: no such
// run, or it found nothing for module from t.
func (a *App) FoundValues(ctx context.Context, actor Actor, workspace string, runID uuid.UUID, module string, t templates.Template) (map[string]any, error) {
	if _, err := a.Run(ctx, actor, workspace, runID, LevelOperate); err != nil {
		return nil, err
	}
	all, err := a.Store.RunFound(ctx, runID)
	if err != nil {
		return nil, err
	}
	f, ok := all[module]
	found := map[string]any{}
	for _, in := range t.Inputs {
		if v, set := f.Inputs[in.Name]; ok && set && !in.Sensitive && f.TemplateID == t.ID {
			found[in.Name] = v
		}
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return found, nil
}

// FoundTemplate is a template module an import plan found values for, and whether it's still in the workspace
// (a discarded or refused run removes a template it added).
type FoundTemplate struct {
	Module, TemplateID string
	Added              bool
}

// FoundTemplates lists what run d (RunDetail checked the actor's access) found values for, by module name.
func (a *App) FoundTemplates(ctx context.Context, d RunDetail) ([]FoundTemplate, error) {
	all, err := a.Store.RunFound(ctx, d.Run.ID)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	added, err := a.Store.WorkspaceTemplates(ctx, d.Workspace.ID)
	var out []FoundTemplate
	for _, m := range slices.Sorted(maps.Keys(all)) {
		out = append(out, FoundTemplate{Module: m, TemplateID: all[m].TemplateID,
			Added: slices.ContainsFunc(added, func(t storage.WorkspaceTemplate) bool { return t.Name == m && t.TemplateID == all[m].TemplateID })})
	}
	return out, err
}

// RemoveTemplate removes an added template and queues the plan that destroys its resources; with
// none deployed it's removed at once and no run is queued (Run.ID uuid.Nil). See storage.RemoveWorkspaceTemplate.
func (a *App) RemoveTemplate(ctx context.Context, actor Actor, workspace, module string) (storage.Run, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return storage.Run{}, err
	}
	run, err := a.Store.RemoveWorkspaceTemplate(ctx, ws.ID, module, actor.UserID)
	if err != nil {
		return run, err
	}
	a.Log.Info("workspace template removed", "workspace", ws.Name, "module", module, "run", run.ID, "user", actor.Email)
	return run, nil
}

// moduleName makes a Terraform module call name (^[a-z][a-z0-9_-]{0,62}$, not "new": the add route)
// from a template's key value or display name: "Virtual machine (Windows)" → "virtual_machine_windows".
func moduleName(name string) string {
	s := strings.Trim(nonModuleChars.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "t_" + s
	}
	s = strings.TrimRight(s[:min(len(s), 63)], "_")
	if s == "new" {
		return "template"
	}
	return s
}

var nonModuleChars = regexp.MustCompile(`[^a-z0-9]+`)

// defaultModule is the name a template is added under, fixed from then on: from its key input's
// form value (raw), else its display name, plus a random hex suffix (_c8ad), drawn again while
// taken: a counter (_2) would read like part of the resource's own name.
func defaultModule(t templates.Template, raw map[string]string, taken []storage.WorkspaceTemplate) string {
	base := moduleName(t.Name)
	if k := t.KeyInput(); k != nil && strings.TrimSpace(raw[k.Name]) != "" {
		base = moduleName(raw[k.Name])
	}
	base = base[:min(len(base), 58)]
	for {
		name := base + "_" + uuid.NewString()[:4]
		if !slices.ContainsFunc(taken, func(a storage.WorkspaceTemplate) bool { return a.Name == name }) {
			return name
		}
	}
}
