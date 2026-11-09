package httpserver

import (
	"cmp"
	"net/http"
	"regexp"
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
	F         *formState // a refused submit
	Workspace string
	AutoApply bool
	Project   string             // the project picked for the new workspace; "" is the default
	Projects  []storage.Project  // to pick from, when creating a workspace
	Configure *storage.Workspace // set: change this workspace's inputs and module version instead of creating one
	Module    string             // with Configure: the added template to change (or add, with Adding)
	Adding    bool
	Cur       templates.Module // with Configure: the module version it runs now
	Set       map[string]bool  // sensitive inputs with a stored value: left empty, it's kept
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
	s.render(w, r, http.StatusOK, "form.html", &formPage{Page: Page{Section: "templates"}, T: t, Editor: s.canEditTemplates(r), Values: vals,
		Project: r.URL.Query().Get("project"), Projects: projects})
}
