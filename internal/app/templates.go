package app

import (
	"cmp"
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

// TemplateAt is catalog template id as it was at module version source/ref (resolved from that version's
// variables): what a workspace that pins that version runs. ok false: id isn't in the catalog anymore.
func (a *App) TemplateAt(ctx context.Context, id, source, ref string) (t templates.Template, ok bool, err error) {
	if t, ok = a.Templates.Get(id); !ok {
		return t, false, nil
	}
	t.Module = templates.Module{Source: source, Ref: ref}
	t, err = a.Resolve(ctx, t)
	return t, true, err
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
	t, adding := f.Template, f.Current == nil
	s := formSave{T: t, Raw: raw, Found: found, Workspace: f.Workspace.ID, Module: module}
	if adding {
		s.Module = defaultModule(t, raw, f.Added)
	} else {
		s.Old, s.Follows = f.Current.Inputs, f.Current.Follows
	}
	module = s.Module
	if s.Env, err = a.ProjectEnvironment(ctx, f.Workspace.ProjectID); err != nil {
		return module, storage.Run{}, err
	}
	sv, err := a.save(s)
	if err != nil {
		return module, storage.Run{}, err
	}
	if len(sv.Errs) > 0 {
		return module, storage.Run{}, &ValidationError{Fields: sv.Errs}
	}
	run, err := a.Store.SetWorkspaceTemplateAs(ctx, f.Workspace.ID, storage.WorkspaceTemplate{Name: module, TemplateID: t.ID,
		ModuleSource: t.Module.Source, ModuleRef: t.Module.Ref, Inputs: sv.Vals, Follows: sv.Follows, Import: sv.Import}, adding, runAction(sv.Import), actor.UserID)
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

// runAction is the action of the run a save queues: an import is planned first (rounds of plans, never applied,
// until AdoptImport applies one), anything else applied.
func runAction(importing bool) string {
	if importing {
		return "plan"
	}
	return "apply"
}

// ImportInput is the input with which a template's module imports what exists instead of creating it (a bool);
// an applied import switches it off again (storage.FinishJob).
const ImportInput = templates.ImportInput

// CanImport reports whether t's module imports what exists when asked (ImportInput, a bool input).
func CanImport(t templates.Template) bool {
	return slices.ContainsFunc(t.Inputs, func(in templates.Input) bool { return in.Name == ImportInput && in.Type == "bool" })
}

// ForImport is t while its module imports: the plan finds the settings, so only the key input (and sensitive ones,
// which the plan never fills) stays required.
func ForImport(t templates.Template) templates.Template {
	t.Inputs = slices.Clone(t.Inputs)
	for i, in := range t.Inputs {
		t.Inputs[i].Required = in.Required && (in.Key || in.Sensitive)
	}
	return t
}

// Importing reports whether raw (form values) has t's module import (ImportInput on; posted only when shown).
func Importing(t templates.Template, raw map[string]string) bool {
	return CanImport(t) && (raw[ImportInput] == "on" || raw[ImportInput] == "true")
}

// validate is t.Validate, except while the module imports (ForImport); a required input left empty is null then
// (the module call needs the argument).
func validate(t templates.Template, raw map[string]string) (map[string]any, map[string]string) {
	if !Importing(t, raw) {
		return t.Validate(raw)
	}
	vals, errs := ForImport(t).Validate(raw)
	for _, in := range t.Inputs {
		if _, set := vals[in.Name]; in.Required && !set && errs[in.Name] == "" && t.Match(in.ShowIf, raw) {
			vals[in.Name] = nil
		}
	}
	return vals, errs
}

// FoundValues is what import plan runID of the workspace found for its template module (added from t; the workspace
// may not have it, when the run ended without its apply): the real values of t's inputs that alone set an imported attribute
// (runnerapi.FinishRequest.Found), over the inputs the plan ran with (what the user set by hand, such as a name the
// module builds from several inputs, so adding the template again keeps it); typed as stored, sensitive inputs
// never. Needs operate. ErrNotFound: no such run, or it found nothing for module from t.
func (a *App) FoundValues(ctx context.Context, actor Actor, workspace string, runID uuid.UUID, module string, t templates.Template) (map[string]any, error) {
	if _, err := a.Run(ctx, actor, workspace, runID, LevelOperate); err != nil {
		return nil, err
	}
	if pending, err := a.Store.RunPending(ctx, runID); err != nil || !pending { // an old link would undo what was applied since
		return nil, cmp.Or(err, ErrNotFound)
	}
	all, err := a.Store.RunFound(ctx, runID)
	if err != nil {
		return nil, err
	}
	f := all[module]
	found, ok := foundValues(f, t)
	if !ok {
		return nil, ErrNotFound
	}
	if f.Import && CanImport(t) { // the run imported: the form goes on importing
		found[ImportInput] = true
	}
	return found, nil
}

// foundValues are the values of t's inputs in f (what a plan found for a module from t): what it found where the
// input takes it, else what the plan ran with; sensitive inputs never. ok false: f isn't from t, or holds nothing.
func foundValues(f storage.Found, t templates.Template) (map[string]any, bool) {
	if f.TemplateID != t.ID || len(f.Inputs)+len(f.Saved) == 0 {
		return nil, false
	}
	found := map[string]any{}
	for _, in := range t.Inputs {
		v, set := f.Inputs[in.Name]
		if set && !fits(in, v) { // the plan's value isn't one the input takes (a map behind a string input): what was entered
			set = false
		}
		if !set {
			v, set = f.Saved[in.Name]
		}
		if set && !in.Sensitive && !IsSealed(v) {
			found[in.Name] = v
		}
	}
	return found, true
}

// fits: v (a decoded JSON value an import plan found) is of the kind input in takes: a value the form can show
// and Validate can check, not, say, a whole map an attribute was set from.
func fits(in templates.Input, v any) bool {
	switch v.(type) {
	case string:
		return in.Type == "" || in.Type == "string" || in.Type == "select"
	case float64:
		return in.Type == "number" || in.Type == "string" // a numeric string attribute read back as a number
	case bool:
		return in.Type == "bool"
	case []any:
		return in.RowFields() != nil
	case map[string]any:
		return in.Type == "object" || in.Type == "map"
	}
	return false
}

// ChangeSettings goes back from run id to its template's form: a plan still waiting for confirmation is discarded
// (which removes a template the run added), and the run's templates are listed (FoundTemplates) to open the form
// with what the run had and what its plan found importing. Needs operate. ErrNotFound: the run didn't change a
// template (or applied it), and found nothing; a conflict while it's still planning.
func (a *App) ChangeSettings(ctx context.Context, actor Actor, workspace string, id uuid.UUID) ([]FoundTemplate, error) {
	d, err := a.Run(ctx, actor, workspace, id, LevelOperate)
	if err != nil {
		return nil, err
	}
	if found, err := a.FoundTemplates(ctx, d); err != nil || len(found) == 0 {
		return nil, cmp.Or(err, ErrNotFound)
	}
	if d.Run.Active() {
		if err := a.DiscardRun(ctx, actor, workspace, id); err != nil {
			return nil, err
		}
	}
	return a.FoundTemplates(ctx, d)
}

// FoundTemplate is a template module a run can go back to the form of (Change settings), whether it's still in
// the workspace (a run that ends before its apply removes a template it added) and whether its plan read what it
// imports.
type FoundTemplate struct {
	Module, TemplateID string
	Added, Read        bool
}

// FoundTemplates lists the template modules run d (RunDetail checked the actor's access) can go back to the form of
// (storage.Found), by name; none once it applied.
func (a *App) FoundTemplates(ctx context.Context, d RunDetail) ([]FoundTemplate, error) {
	if pending, err := a.Store.RunPending(ctx, d.Run.ID); err != nil || !pending {
		return nil, err
	}
	all, err := a.Store.RunFound(ctx, d.Run.ID)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	added, err := a.Store.WorkspaceTemplates(ctx, d.Workspace.ID)
	var out []FoundTemplate
	for _, m := range slices.Sorted(maps.Keys(all)) {
		out = append(out, FoundTemplate{Module: m, TemplateID: all[m].TemplateID, Read: len(all[m].Inputs) > 0,
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
