package app

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
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
// plan that applies it. Adding, adopt are the IDs of existing resources to import (see adoptions;
// ignored when configuring). It returns the module name. ValidationError: the inputs (Fields by input
// name) and IDs (by templates.AdoptField).
func (a *App) SaveTemplate(ctx context.Context, actor Actor, workspace, module, templateID string, raw map[string]string, adopt []string) (string, storage.Run, error) {
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
	if adding {
		raw = withPlaceholders(t, raw, adopt)
	}
	vals, errs := t.Validate(raw)
	if err := a.SealInputs(t, raw, vals, old, errs, f.Workspace.ID, module); err != nil {
		return module, storage.Run{}, err
	}
	keepOwn(vals, errs, locked, old)
	var imports []runnerapi.Import
	if adding {
		imports = adoptions(t, raw, adopt, module, errs)
	}
	if err := a.refuseManaged(ctx, t, module, imports, errs); err != nil {
		return module, storage.Run{}, err
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
		"ref", t.Module.Ref, "run", run.ID, "adopts", len(imports), "user", actor.Email)
	return module, run, nil
}

// adoptions checks the IDs pasted to adopt existing resources into template t added as module (raw: the
// form values, for show_if), adds their errors to errs, and returns the run's imports, sorted by address
// (nil: adopt nothing).
func adoptions(t templates.Template, raw map[string]string, ids []string, module string, errs map[string]string) []runnerapi.Import {
	byAddr, idErrs := t.AdoptIDs(raw, ids)
	maps.Copy(errs, idErrs)
	var out []runnerapi.Import
	for _, to := range slices.Sorted(maps.Keys(byAddr)) {
		out = append(out, runnerapi.Import{To: "module." + module + "." + to, ID: byAddr[to]})
	}
	return out
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

// refuseManaged adds an error for each of imports whose ID a workspace's state manages already: two states
// must not manage one resource. IDs compare in any case.
func (a *App) refuseManaged(ctx context.Context, t templates.Template, module string, imports []runnerapi.Import, errs map[string]string) error {
	if len(imports) == 0 {
		return nil
	}
	ids := make([]string, len(imports))
	for i, im := range imports {
		ids[i] = im.ID
	}
	managed, err := a.Store.ManagedBy(ctx, ids)
	for _, im := range imports {
		if ws := managed[strings.ToLower(im.ID)]; ws != "" {
			i := slices.IndexFunc(t.Adopt, func(e templates.Adopt) bool { return "module."+module+"."+e.To == im.To })
			errs[templates.AdoptField(i)] = "Already managed in workspace " + ws + ": a resource can be in one state only."
		}
	}
	return err
}

// withPlaceholders fills the placeholder inputs of t's adopt entries that get an ID (adopting: ids, as for
// SaveTemplate) with a random value when left empty: values the cloud never returns, such as an admin password,
// which the module must ignore.
func withPlaceholders(t templates.Template, raw map[string]string, ids []string) map[string]string {
	byAddr, _ := t.AdoptIDs(raw, ids)
	if byAddr == nil {
		return raw
	}
	raw = maps.Clone(raw)
	for _, a := range t.Adopt {
		if byAddr[a.To] == "" {
			continue
		}
		for _, in := range a.Placeholder {
			if strings.TrimSpace(raw[in]) == "" {
				raw[in] = "Ops-" + rand.Text() + "-a1!" // letters, digits and a symbol: passes common complexity rules
			}
		}
	}
	return raw
}

// StartScan queues a scan of the workspace, with its credentials, for existing resources templateID can adopt
// (runner.scan). Needs operate. ErrNotFound: no such template; ValidationError: it has no adopt entry with list.
func (a *App) StartScan(ctx context.Context, actor Actor, workspace, templateID string) (storage.Workspace, storage.Run, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return ws, storage.Run{}, err
	}
	t, err := a.Template(ctx, templateID)
	if err != nil {
		return ws, storage.Run{}, err
	}
	if !t.Scannable() {
		return ws, storage.Run{}, invalid("template", t.Name+" can't scan: none of its adopt entries says how to find resources (list)")
	}
	run, err := a.Store.CreateScan(ctx, ws.ID, t.ID, actor.UserID)
	if err != nil {
		return ws, run, err
	}
	a.Log.Info("scan queued", "workspace", ws.Name, "template", t.ID, "run", run.ID, "user", actor.Email)
	return ws, run, nil
}

// ScanResult is what a scan found: its template (by id only if it left the catalog) and the candidates, each
// with the workspace already managing it.
type ScanResult struct {
	Template   templates.Template
	Candidates []ScanCandidate
}

type ScanCandidate struct {
	runnerapi.Candidate
	ManagedBy string // a workspace whose current state has its ID; "" none
}

// ScanResult loads scan run runID's result (RunDetail checked the actor's access).
func (a *App) ScanResult(ctx context.Context, runID uuid.UUID) (ScanResult, error) {
	sc, err := a.Store.RunScan(ctx, runID)
	if err != nil {
		return ScanResult{}, err
	}
	res := ScanResult{Template: templates.Template{ID: sc.Template, Name: sc.Template}}
	if t, ok := a.Templates.Get(sc.Template); ok {
		res.Template = t
	}
	ids := make([]string, len(sc.Candidates))
	for i, c := range sc.Candidates {
		ids[i] = c.ID
	}
	managed, err := a.Store.ManagedBy(ctx, ids)
	for _, c := range sc.Candidates {
		res.Candidates = append(res.Candidates, ScanCandidate{c, managed[strings.ToLower(c.ID)]})
	}
	return res, err
}

// ScanCandidate returns candidate n of the workspace's scan run runID, to fill the add form. Needs operate.
func (a *App) ScanCandidate(ctx context.Context, actor Actor, workspace string, runID uuid.UUID, n int) (runnerapi.Candidate, error) {
	d, err := a.Run(ctx, actor, workspace, runID, LevelOperate)
	if err != nil {
		return runnerapi.Candidate{}, err
	}
	sc, err := a.Store.RunScan(ctx, d.Run.ID)
	if err != nil {
		return runnerapi.Candidate{}, err
	}
	if n < 0 || n >= len(sc.Candidates) {
		return runnerapi.Candidate{}, ErrNotFound
	}
	return sc.Candidates[n], nil
}
