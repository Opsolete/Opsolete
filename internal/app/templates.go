package app

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
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
// plan that applies it. Adding, from (nil: none) is a scan's result to import (Import): the plan imports
// its resources instead of creating them. It returns the module name. ValidationError: the inputs
// (Fields by input name), or the import.
func (a *App) SaveTemplate(ctx context.Context, actor Actor, workspace, module, templateID string, raw map[string]string, from *Found) (string, storage.Run, error) {
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
	locked := env.Locked(t)
	raw = lockInputs(raw, locked)
	var c *runnerapi.Candidate
	if adding && from != nil {
		if c, err = a.importing(ctx, actor, f.Workspace, t, *from); err != nil {
			return module, storage.Run{}, err
		}
		raw = withPlaceholders(raw, c.Placeholder)
	}
	vals, errs := t.Validate(raw)
	if err := a.SealInputs(t, raw, vals, old, errs, f.Workspace.ID, module); err != nil {
		return module, storage.Run{}, err
	}
	keepOwn(vals, errs, locked, old)
	var imports []runnerapi.Import
	if c != nil {
		for _, im := range c.Imports {
			imports = append(imports, runnerapi.Import{To: "module." + module + "." + im.To, ID: im.ID})
		}
	}
	if len(errs) > 0 {
		return module, storage.Run{}, &ValidationError{Fields: errs}
	}
	run, err := a.Store.SetWorkspaceTemplate(ctx, f.Workspace.ID, storage.WorkspaceTemplate{Name: module, TemplateID: t.ID,
		ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: vals, Imports: imports}, adding, actor.UserID)
	if errors.Is(err, storage.ErrExists) {
		return module, run, &ValidationError{Message: "this workspace already has a template named " + module + " (added meanwhile), try again"}
	}
	if err != nil {
		return module, run, err
	}
	a.Log.Info("workspace template saved", "workspace", f.Workspace.Name, "module", module, "template", t.ID, "added", adding,
		"ref", t.Module.Ref, "run", run.ID, "imports", len(imports), "user", actor.Email)
	return module, run, nil
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
