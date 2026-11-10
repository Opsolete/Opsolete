package app

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
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

// Importing existing resources (a workspace's Import): a scan run finds, with the workspace's credentials,
// resources the catalog's templates can take over, and one at a time each is added as its template, the form
// filled in from what was found, with a plan that imports the resources instead of creating them. What a
// template can find, and how that fills its form, is read from its registry module's code (registry.Adoptable):
// templates need nothing for it.

// importable is a catalog template a scan can find resources for.
type importable struct {
	t       templates.Template // resolved
	entries []runnerapi.ScanEntry
	notes   []string
}

// importables reads what a scan can find for each catalog template; skipped says which can't take part and why.
func (a *App) importables(ctx context.Context) (out []importable, skipped []string, err error) {
	for _, t := range a.Templates.List() {
		if t, err = a.Resolve(ctx, t); err != nil {
			return nil, nil, err
		}
		ns, name, sys, ok := a.Registry.ParseSource(t.Module.Source)
		if !ok {
			skipped = append(skipped, t.Name+": its module isn't in the registry, so its code can't be read")
			continue
		}
		archive, err := a.Store.ModuleArchive(ctx, ns, name, sys, t.Module.Ref)
		if errors.Is(err, storage.ErrNotFound) {
			skipped = append(skipped, t.Name+": its module version isn't in the registry")
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		entries, notes, err := registry.Adoptable(archive, t.Inputs)
		if err != nil {
			skipped = append(skipped, t.Name+": reading its module: "+err.Error())
			continue
		}
		if !slices.ContainsFunc(entries, runnerapi.ScanEntry.Primary) {
			skipped = append(skipped, t.Name+": its module has no resource to find")
			continue
		}
		out = append(out, importable{t, entries, notes})
	}
	return out, skipped, nil
}

// ImportPage is a workspace's Import page: what can be imported, and the latest scans.
type ImportPage struct {
	Workspace storage.Workspace
	Level     Level
	Templates []templates.Template // the ones a scan finds resources for
	Skipped   []string             // the others, and why
	Scans     []storage.Run        // the latest, newest first
}

// ImportPage loads the workspace's Import page. Needs read.
func (a *App) ImportPage(ctx context.Context, actor Actor, workspace string) (ImportPage, error) {
	var p ImportPage
	var err error
	if p.Workspace, p.Level, err = a.Workspace(ctx, actor, workspace, LevelRead); err != nil {
		return p, err
	}
	imps, skipped, err := a.importables(ctx)
	if err != nil {
		return p, err
	}
	for _, i := range imps {
		p.Templates = append(p.Templates, i.t)
	}
	p.Skipped = skipped
	runs, err := a.Store.Runs(ctx, p.Workspace.ID, 50)
	for _, r := range runs {
		if r.Action == "scan" && len(p.Scans) < 5 {
			p.Scans = append(p.Scans, r)
		}
	}
	return p, err
}

// rgRe is what Azure allows in a resource group name (letters, digits, _ - . ( )).
var rgRe = regexp.MustCompile(`^[-\w.()]{1,90}$`)

// FieldResourceGroup is the Import form's resource group field.
const FieldResourceGroup = "resource_group"

// StartScan queues a scan of the workspace, with its credentials, of the resources in resourceGroup ("" the whole
// subscription): what the catalog's templates can take over, and what not. Needs operate. ValidationError: the
// resource group's name.
func (a *App) StartScan(ctx context.Context, actor Actor, workspace, resourceGroup string) (storage.Workspace, storage.Run, error) {
	ws, _, err := a.Workspace(ctx, actor, workspace, LevelOperate)
	if err != nil {
		return ws, storage.Run{}, err
	}
	if resourceGroup = strings.TrimSpace(resourceGroup); resourceGroup != "" && !rgRe.MatchString(resourceGroup) {
		return ws, storage.Run{}, invalid(FieldResourceGroup, "A resource group name has 1-90 letters, digits and _ - . ( ).")
	}
	run, err := a.Store.CreateScan(ctx, ws.ID, resourceGroup, actor.UserID)
	if err != nil {
		return ws, run, err
	}
	a.Log.Info("scan queued", "workspace", ws.Name, "resource_group", resourceGroup, "run", run.ID, "user", actor.Email)
	return ws, run, nil
}

// scanJob is a scan run's job part: what to find for which template, each template's module named t<i>.
func (a *App) scanJob(ctx context.Context, resourceGroup string) (*runnerapi.Scan, []runnerapi.Module, error) {
	imps, skipped, err := a.importables(ctx)
	if err != nil {
		return nil, nil, err
	}
	sc := &runnerapi.Scan{ResourceGroup: resourceGroup, Templates: []runnerapi.ScanTemplate{}, Notes: skipped}
	var mods []runnerapi.Module
	for _, imp := range imps {
		b, err := json.Marshal(imp.t)
		if err != nil {
			return nil, nil, err
		}
		name := "t" + uuid.NewString()[:8]
		sc.Templates = append(sc.Templates, runnerapi.ScanTemplate{Template: b, Module: name, Entries: imp.entries, Notes: imp.notes})
		mods = append(mods, runnerapi.Module{Name: name, Source: a.Registry.RunnerSource(imp.t.Module, runnerSrcTTL)})
	}
	return sc, mods, nil
}

// ScanResult is what a scan found, in four groups: what a template can import, what its template would replace
// (the template or module needs work), what no template can import, and what a workspace manages already.
// Resources a candidate imports or uses (its NIC, its OS disk) are listed with it only.
type ScanResult struct {
	ResourceGroup string
	Stocked       bool // it took stock of every resource, not only what templates find
	Total         int  // resources it saw
	Importable    []ScanCandidate
	Rework        []ScanCandidate
	NoTemplate    []ScanRow
	Managed       []ScanRow
}

type ScanCandidate struct {
	runnerapi.Candidate
	N            int      // its index in the scan, for Import
	TemplateName string   // its template's name; its id if it left the catalog
	Related      []string // the other resources it imports, e.g. "network interface testvm807"
	Uses         []string // resources it refers to without importing them, e.g. "disks testvm_OsDisk_1"
}

// ScanRow is a resource listed on its own: no template can import it, or a workspace manages it.
type ScanRow struct {
	Name, Type, ResourceGroup, ID string
	ManagedBy                     string // the workspace; "" none
}

// ScanResult loads scan run runID's result (RunDetail checked the actor's access).
func (a *App) ScanResult(ctx context.Context, runID uuid.UUID) (ScanResult, error) {
	sc, err := a.Store.RunScan(ctx, runID)
	if err != nil {
		return ScanResult{}, err
	}
	res := ScanResult{ResourceGroup: sc.ResourceGroup, Stocked: sc.Inventory != nil, Total: len(sc.Inventory)}
	byID := map[string]runnerapi.Found{}
	ids := make([]string, 0, len(sc.Inventory))
	for _, f := range sc.Inventory {
		byID[strings.ToLower(f.ID)] = f
		ids = append(ids, f.ID)
	}
	covered := map[string]bool{} // listed with a candidate
	for _, c := range sc.Candidates {
		for _, im := range c.Imports {
			ids, covered[strings.ToLower(im.ID)] = append(ids, im.ID), true
		}
		for _, u := range c.Uses {
			covered[strings.ToLower(u)] = true
		}
	}
	managed, err := a.Store.ManagedBy(ctx, ids)
	if err != nil {
		return res, err
	}
	for n, c := range sc.Candidates {
		cand := ScanCandidate{Candidate: c, N: n, TemplateName: c.Template}
		if t, ok := a.Templates.Get(c.Template); ok {
			cand.TemplateName = t.Name
		}
		var by string
		for _, im := range c.Imports {
			by = cmp.Or(by, managed[strings.ToLower(im.ID)])
			if !strings.EqualFold(im.ID, c.ID) {
				cand.Related = append(cand.Related, kind(im.To)+" "+lastSegment(im.ID))
			}
		}
		for _, u := range c.Uses {
			f := byID[strings.ToLower(u)]
			cand.Uses = append(cand.Uses, lastSegment(f.Type)+" "+cmp.Or(f.Name, lastSegment(u)))
		}
		switch {
		case by != "":
			f := byID[strings.ToLower(c.ID)]
			res.Managed = append(res.Managed, ScanRow{Name: c.Name, Type: cmp.Or(f.Type, kind(c.Imports[len(c.Imports)-1].To)),
				ResourceGroup: f.ResourceGroup, ID: c.ID, ManagedBy: by})
		case c.Fit == "replaces":
			res.Rework = append(res.Rework, cand)
		default:
			res.Importable = append(res.Importable, cand)
		}
	}
	for _, f := range sc.Inventory {
		if covered[strings.ToLower(f.ID)] {
			continue
		}
		row := ScanRow{Name: f.Name, Type: f.Type, ResourceGroup: f.ResourceGroup, ID: f.ID, ManagedBy: managed[strings.ToLower(f.ID)]}
		if row.ManagedBy != "" {
			res.Managed = append(res.Managed, row)
		} else {
			res.NoTemplate = append(res.NoTemplate, row)
		}
	}
	for _, rows := range [][]ScanRow{res.NoTemplate, res.Managed} {
		slices.SortFunc(rows, func(a, b ScanRow) int {
			return cmp.Or(strings.Compare(a.Type, b.Type), strings.Compare(a.ResourceGroup, b.ResourceGroup), strings.Compare(a.Name, b.Name))
		})
	}
	return res, nil
}

func lastSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// kind names the resource type of an address in words: azurerm_network_interface.self → network interface.
func kind(addr string) string {
	typ, _, _ := strings.Cut(addr, ".")
	if _, rest, ok := strings.Cut(typ, "_"); ok {
		typ = rest
	}
	return strings.ReplaceAll(typ, "_", " ")
}

// Found is one result of a scan run: run's candidate N.
type Found struct {
	Run uuid.UUID
	N   int
}

// ScanCandidate returns what the workspace's scan run found as from.N, to fill its template's form. Needs operate.
func (a *App) ScanCandidate(ctx context.Context, actor Actor, workspace string, from Found) (runnerapi.Candidate, error) {
	d, err := a.Run(ctx, actor, workspace, from.Run, LevelOperate)
	if err != nil {
		return runnerapi.Candidate{}, err
	}
	sc, err := a.Store.RunScan(ctx, d.Run.ID)
	if err != nil {
		return runnerapi.Candidate{}, err
	}
	if from.N < 0 || from.N >= len(sc.Candidates) {
		return runnerapi.Candidate{}, ErrNotFound
	}
	return sc.Candidates[from.N], nil
}

// importing checks that from, found by a scan of workspace ws, can be imported as template t: its template, and
// none of its resources managed by a workspace already (a resource in two states, or twice in one, is destroyed
// by removing either; the plan checks it again against the state it reads).
func (a *App) importing(ctx context.Context, actor Actor, ws storage.Workspace, t templates.Template, from Found) (*runnerapi.Candidate, error) {
	c, err := a.ScanCandidate(ctx, actor, ws.Name, from)
	if err != nil {
		return nil, err
	}
	if c.Template != t.ID {
		return nil, &ValidationError{Message: "this resource was found for another template"}
	}
	ids := make([]string, len(c.Imports))
	for i, im := range c.Imports {
		ids[i] = im.ID
	}
	managed, err := a.Store.ManagedBy(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range slices.Sorted(maps.Keys(managed)) {
		return nil, &ValidationError{Message: id + " is managed already, in workspace " + managed[id] + ": a resource can be in one state only"}
	}
	return &c, nil
}

// withPlaceholders fills the inputs names left empty with a random value: values the cloud never returns, such
// as an admin password, which the module ignores (lifecycle ignore_changes).
func withPlaceholders(raw map[string]string, names []string) map[string]string {
	raw = maps.Clone(raw)
	for _, in := range names {
		if strings.TrimSpace(raw[in]) == "" {
			raw[in] = "Ops-" + rand.Text() + "-a1!" // letters, digits and a symbol: passes common complexity rules
		}
	}
	return raw
}
