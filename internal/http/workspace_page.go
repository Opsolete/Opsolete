package httpserver

import (
	"cmp"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// workspacePage is a workspace's side-menu page (workspace.html): the header part every tab shows,
// and the part of its tab (Page.Tab), the only one of the embedded tab parts that is set.
type workspacePage struct {
	Page
	W          storage.Workspace
	Lock       *storage.StateLock
	Runs       []storage.Run // the latest 25
	Active     *storage.Run  // the run (or drift check) holding the workspace
	Current    *runItem      // the latest run
	Resources  []storage.Resource
	Versions   []storage.StateVersion // the latest 10
	Outputs    map[string]any
	Destroyed  bool
	Assessment *storage.Run // the latest drift check
	F          *formState   // a refused form on the page
	*overviewTab
	*deployTab
	*runsTab
	*statesTab
	*variablesTab
	*settingsTab
}

type overviewTab struct {
	Added          []storage.WorkspaceTemplate
	Updates        map[string]string // added template name -> module version its template pins now
	TemplateNames  map[string]string // template id -> display name; a deleted template shows its id
	ResourceGroups []resourceGroup
	LatestPlan     phase
	LatestApply    phase
	Metrics        storage.RunMetrics
	Show           string // ?show=: resources (the default) or outputs
}

// deployTab is the catalog, each template one click from its add form.
type deployTab struct{ Catalog []templates.Template }

// runsTab: status filters, counted over the runs listed.
type runsTab struct {
	Filter  string
	Counts  map[string]int
	RunList []runItem
}

// statesTab is the version list, or with State one version: what it changed, and what rolling back to it would change.
type statesTab struct {
	State           *storage.StateVersion
	Prev            *storage.StateVersion
	Changes         []app.StateChange
	IsCurrent       bool
	RollbackChanges []app.StateChange
}

// variablesTab: the workspace's own variables, and each applied set with its variables, read-only
// (sets are edited by admins on their page).
type variablesTab struct {
	Variables varsBlock
	Sets      []setView
}

// workspaceTab serves the {name} workspace's menu page tab; "settings" and "states" also serve their
// sub-pages ({page}, {version}).
func (s *Server) workspaceTab(tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.renderWorkspace(w, r, tab, r.PathValue("page"), http.StatusOK, nil)
	}
}

// onWorkspacePage re-renders a refused form on the {name} workspace's page tab, settings sub-page sub.
func (s *Server) onWorkspacePage(w http.ResponseWriter, r *http.Request, tab, sub string) func(int, *formState) {
	return func(status int, f *formState) { s.renderWorkspace(w, r, tab, sub, status, f) }
}

// renderWorkspace renders the {name} workspace's page tab (sub: the settings sub-page, "" General);
// f: a refused form on it.
func (s *Server) renderWorkspace(w http.ResponseWriter, r *http.Request, tab, sub string, status int, f *formState) {
	if !slices.Contains([]string{"", "health", "lock", "access", "deletion"}, sub) {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// each tab loads only what it shows (the header's data on every one)
	need := app.LevelRead
	if tab == "settings" { // readers see what they need of the settings on the overview
		need = app.LevelOperate
	}
	st, err := s.App.State(ctx, actor(r), r.PathValue("name"), need) // the header shows the state on every page
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	ws, level := st.Workspace, st.Level
	runs, err := st.Runs(ctx, 25)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	res, err := st.Resources(ctx)
	if err != nil {
		s.internalError(w, "workspace resources", err)
		return
	}
	versions, err := st.Versions(ctx, 10)
	if err != nil {
		s.internalError(w, "state versions", err)
		return
	}
	outputs, destroyed, err := st.Outputs(ctx)
	if err != nil {
		s.internalError(w, "outputs", err)
		return
	}
	var active *storage.Run
	if len(runs) > 0 && runs[0].Active() {
		active = &runs[0]
	}
	assessment, err := st.LatestAssessment(ctx)
	if err != nil {
		s.internalError(w, "latest assessment", err)
		return
	}
	if assessment != nil && assessment.Active() { // a drift check holds the workspace like any run
		active = assessment
	}
	var current *runItem
	if len(runs) > 0 {
		current = &runItem{runs[0], true}
	}
	lock, err := st.CurrentLock(ctx)
	if err != nil {
		s.internalError(w, "state lock", err)
		return
	}
	p := &workspacePage{Page: workspaceNav(r.PathValue("name"), tab, sub, level == app.LevelOperate), W: ws, Lock: lock, Runs: runs, Active: active, Current: current,
		Resources: res, Versions: versions, Outputs: outputs, Destroyed: destroyed, Assessment: assessment, F: f}

	switch tab {
	case "overview":
		added, err := st.Templates(ctx)
		if err != nil {
			s.internalError(w, "workspace templates", err)
			return
		}
		o := &overviewTab{Added: added, Updates: map[string]string{}, TemplateNames: map[string]string{}, Show: "resources"}
		if r.URL.Query().Get("show") == "outputs" {
			o.Show = "outputs"
		}
		for _, a := range added {
			cur, ok := s.App.Templates.Get(a.TemplateID)
			if ok {
				o.TemplateNames[cur.ID] = cur.Name
			}
			if ok && (cur.Module.Source != a.ModuleSource || cur.Module.Ref != a.ModuleRef) {
				o.Updates[a.Name] = cur.Module.Ref
			}
		}
		if len(runs) > 0 {
			d, err := s.App.Run(ctx, actor(r), ws.Name, runs[0].ID, app.LevelRead)
			if err != nil {
				s.uiError(w, r, err)
				return
			}
			o.LatestPlan, o.LatestApply = runPhases(runs[0], d.History)
		}
		if o.Metrics, err = st.RunMetrics(ctx); err != nil {
			s.internalError(w, "run metrics", err)
			return
		}
		o.ResourceGroups = groupResources(res, added, o.TemplateNames, o.Updates)
		p.overviewTab = o

	case "deploy":
		p.deployTab = &deployTab{Catalog: s.App.Templates.List()}

	case "runs":
		t := &runsTab{Filter: r.URL.Query().Get("filter"), Counts: map[string]int{"all": len(runs)}, RunList: []runItem{}}
		for i, run := range runs {
			g := runGroup(run.Status)
			t.Counts[g]++
			if t.Filter == "" || t.Filter == "all" || t.Filter == g {
				t.RunList = append(t.RunList, runItem{run, i == 0})
			}
		}
		p.runsTab = t

	case "states":
		p.statesTab = &statesTab{}
		if r.PathValue("version") == "" {
			break
		}
		id, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d, err := st.Diff(ctx, id) // operators only
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		p.statesTab = &statesTab{State: &d.Version, Prev: d.Prev, Changes: d.Changes, IsCurrent: d.IsCurrent, RollbackChanges: d.Rollback}

	case "variables":
		wv, err := s.App.WorkspaceVariables(ctx, actor(r), ws.Name)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		t := &variablesTab{Variables: varsBlock{Base: "/workspaces/" + ws.Name, Vars: wv.Own, CanEdit: level == app.LevelOperate, F: formOf(f, "variable")}}
		for _, set := range wv.Sets {
			t.Sets = append(t.Sets, setView{set.VariableSet, varsBlock{Vars: set.Vars}})
		}
		p.variablesTab = t

	case "settings":
		var ok bool
		if p.settingsTab, ok = s.loadSettings(w, r, st, sub); !ok {
			return
		}
	}
	s.render(w, r, status, "workspace.html", p)
}

// resourceGroup is the resources of one top-level module ("" = the root: a VCS branch's), with the template it comes from.
type resourceGroup struct {
	Module, Template, Ref string
	Added                 *storage.WorkspaceTemplate // the added template the module is; nil for the root or a module of the workspace's own
	Update                string                     // the module version its template pins now, when that differs
	Rows                  []resourceRow
}

type resourceRow struct {
	storage.Resource
	Short    string // the address inside the group's module
	Provider string // e.g. azurerm
}

// groupResources groups resources by top-level module (nested modules stay in their parent's group), with a group
// for each added template too, also one without resources in the state (an apply that failed, or none yet), so its
// Configure and Remove are there. Modules by name, the root last. updates: Update by added template name.
func groupResources(res []storage.Resource, added []storage.WorkspaceTemplate, names, updates map[string]string) []resourceGroup {
	var groups []resourceGroup
	idx := map[string]int{}
	group := func(top string) int {
		if i, ok := idx[top]; ok {
			return i
		}
		g := resourceGroup{Module: top}
		call, _, _ := strings.Cut(strings.TrimPrefix(top, "module."), "[") // module.vm["a"] is the call vm
		for i, a := range added {
			if top != "" && a.Name == call {
				g.Template, g.Ref, g.Added, g.Update = cmp.Or(names[a.TemplateID], a.TemplateID), a.ModuleRef, &added[i], updates[a.Name]
			}
		}
		idx[top] = len(groups)
		groups = append(groups, g)
		return idx[top]
	}
	for _, r := range res {
		top, _, _ := strings.Cut(r.Module, ".module.")
		i := group(top)
		short := r.Address
		if top != "" {
			short = strings.TrimPrefix(r.Address, top+".")
		}
		groups[i].Rows = append(groups[i].Rows, resourceRow{r, short, path.Base(r.Provider)})
	}
	for i := range added { // templates nothing in the state belongs to
		if !slices.ContainsFunc(groups, func(g resourceGroup) bool { return g.Added == &added[i] }) {
			group("module." + added[i].Name)
		}
	}
	slices.SortFunc(groups, func(a, b resourceGroup) int {
		switch { // group keys are unique: at most one root
		case a.Module == "":
			return 1
		case b.Module == "":
			return -1
		}
		return cmp.Compare(a.Module, b.Module)
	})
	return groups
}
