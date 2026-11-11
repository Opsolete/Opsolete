package httpserver

import (
	"cmp"
	"github.com/google/uuid"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// The template catalog: the home page lists it, each template's deploy form creates a workspace
// (createWorkspace). Editing is builder.go's, the Git catalog's settings catalog.go's.

// indexPage is the home page (index.html): the catalog.
type indexPage struct {
	Page
	Templates []templates.Template
	Editor    bool // the template builder is on
	CanCreate bool // the user may create workspaces somewhere
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	can, err := s.canCreateWorkspaces(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "index.html", &indexPage{Page{}, s.App.Templates.List(), s.canEditTemplates(r), can})
}

// formPage is the deploy form (form.html): a template's inputs, to create a workspace or configure an added template.
type formPage struct {
	Page
	T         templates.Template
	Editor    bool
	Preview   bool // the builder's unsaved template: shown, never submitted
	Values    map[string]string
	Found     map[string]any // "Use found values": what import plan FoundRun found (app.FoundValues), filled in
	FoundRun  uuid.UUID
	ImportURL string     // creating or adding a template whose module can import: the form importing instead; "" can't
	CreateURL string     // and creating
	Importing bool       // the module imports what exists (app.Importing): only the key input required
	F         *formState // a refused submit
	Workspace string
	AutoApply bool
	Project   string             // the project picked for the new workspace; "" is the default
	Projects  []storage.Project  // to pick from, when creating a workspace
	Configure *storage.Workspace // set: change this workspace's inputs and module version instead of creating one
	Module    string             // with Configure: the added template to change (or add, with Adding)
	Adding    bool
	Cur       templates.Module  // with Configure: the module version it runs now
	Set       map[string]bool   // sensitive inputs with a stored value: left empty, it's kept
	Locked    map[string]string // per-environment inputs the project sets (canonical values by name): read-only, not posted
	LockedBy  string            // that project
	EnvReload bool              // creating: picking another project posts the form with refresh, shown again with that project's values
}

// FoundText is the found value of input name as the form shows it; "" none.
func (p formPage) FoundText(name string) string {
	if v, ok := p.Found[name]; ok {
		return templates.FormatValue(v)
	}
	return ""
}

// importChoice offers creating or importing, when the form adds or deploys a template whose module can import, and
// shows it importing when ?import=1 asks or its values say so (T then marks only the key input required, as the
// server checks it).
func (p *formPage) importChoice(r *http.Request) {
	if !app.CanImport(p.T) || p.Configure != nil && !p.Adding {
		return
	}
	q := r.URL.Query()
	q.Del("import")
	p.CreateURL = r.URL.Path + "?" + q.Encode()
	q.Set("import", "1")
	p.ImportURL = r.URL.Path + "?" + q.Encode()
	if r.Method == http.MethodGet && r.URL.Query().Get("import") == "1" { // opening it; a post says itself
		p.Values = maps.Clone(p.Values)
		p.Values[app.ImportInput] = "true"
	}
	if p.Importing = app.Importing(p.T, p.Values); p.Importing {
		p.T = app.ForImport(p.T)
	}
}

// LockedText is how the form says the project's value of the locked input in.
func (p formPage) LockedText(in templates.Input) string { return app.ValueText(in, p.Locked[in.Name]) }

// lock shows the inputs project sets read-only (app.EnvironmentMap.Locked, the workspace's own values own): their
// values are the project's, also for show_if, and the server ignores what is posted for them.
func (s *Server) lock(r *http.Request, page *formPage, project storage.Project, own map[string]any) error {
	env, err := s.App.ProjectEnvironment(r.Context(), project.ID)
	page.Locked, page.LockedBy = env.Locked(page.T, own), project.Name
	page.Values = maps.Clone(page.Values)
	maps.Copy(page.Values, page.Locked)
	return err
}

// lockDeploy is lock for the deploy form, with the project picked (name, "" the default) if the user may create workspaces there.
func (s *Server) lockDeploy(r *http.Request, page *formPage) error {
	page.EnvReload = len(page.Projects) > 1 && slices.ContainsFunc(page.T.Inputs, func(in templates.Input) bool { return in.PerEnvironment })
	i := slices.IndexFunc(page.Projects, func(p storage.Project) bool { return p.Name == page.Project || page.Project == "" && p.Default })
	if i < 0 {
		return nil
	}
	return s.lock(r, page, page.Projects[i], nil)
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// NamePlaceholder suggests a workspace name from the template's name ("Windows VM" → windows-vm-prod):
// builder-made templates have UUID ids, which read badly.
func (p formPage) NamePlaceholder() string {
	base := cmp.Or(strings.Trim(nonNameChars.ReplaceAllString(strings.ToLower(p.T.Name), "-"), "-"), "workspace")
	return strings.TrimRight(base[:min(len(base), 58)], "-") + "-prod"
}

func (s *Server) form(w http.ResponseWriter, r *http.Request) {
	t, err := s.App.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	vals, _ := app.FormDefaults(t, nil)
	projects, err := s.creatableProjects(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	page := formPage{Page: Page{Section: "templates"}, T: t, Editor: s.canEditTemplates(r), Values: vals,
		Project: r.URL.Query().Get("project"), Projects: projects}
	page.importChoice(r)
	if err := s.lockDeploy(r, &page); err != nil {
		s.internalError(w, "environment values", err)
		return
	}
	s.render(w, r, http.StatusOK, "form.html", &page)
}
