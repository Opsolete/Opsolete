package httpserver

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// The template builder: creates and edits catalog templates (TEMPLATE_EDITOR or a Git catalog).

// builderPage is the template builder (/templates/new, /templates/{id}/edit).
type builderPage struct {
	Page
	T           templates.Template
	Rows        []templates.Input // existing inputs plus blank rows to fill in
	Editing     bool
	F           *formState // a refused save
	Warning     string
	Modules     []storage.Module // registry modules to start from
	Selected    string           // ns/name/system/version picked from the registry
	Registry    bool             // registry enabled: offer "start from a registry module"
	PullRequest string           // the pull request the save opened (Git catalog in pr mode)
}

// ShowOptions lists the fields row i's "show when" can pick: the select and bool rows above it, plus its
// current choice (so an invalid one is shown with the save error, not silently dropped).
func (p builderPage) ShowOptions(i int) []string {
	var names []string
	for _, in := range p.Rows[:i] {
		if (in.Type == "select" || in.Type == "bool") && in.Name != "" {
			names = append(names, in.Name)
		}
	}
	if c := p.Rows[i].ShowIf; c != nil && !slices.Contains(names, c.Input) {
		names = append(names, c.Input)
	}
	return names
}

func (s *Server) renderBuilder(w http.ResponseWriter, r *http.Request, status int, p builderPage) {
	if p.Registry = s.Features.Registry; p.Registry {
		mods, err := s.App.Modules(r.Context())
		if err != nil {
			s.App.Log.Error("list modules", "err", err)
			http.Error(w, "could not load modules", http.StatusInternalServerError)
			return
		}
		p.Modules = mods
	}
	p.Rows = append(slices.Clone(p.T.Inputs), templates.Input{}) // a blank card to fill in (and for "+ Add field" to clone)
	// and a blank option row per card; Clip so the template's own slice is never written
	for i := range p.Rows {
		p.Rows[i].Options = append(slices.Clip(p.Rows[i].Options), templates.Option{})
	}
	p.Page = Page{Section: "templates"}
	s.render(w, r, status, "builder.html", &p)
}

func (s *Server) builder(w http.ResponseWriter, r *http.Request) {
	var p builderPage
	if id := r.PathValue("id"); id != "" {
		t, err := s.App.Template(r.Context(), id)
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		p.T, p.Editing = t, true
	}
	// ?name=&description=: typed before "Use module" reloaded the page
	if q := r.URL.Query(); q.Has("description") {
		p.T.Name, p.T.Description = cmp.Or(strings.TrimSpace(q.Get("name")), p.T.Name), strings.TrimSpace(q.Get("description"))
	}
	// ?module=ns/name/system/version: point the template at a registry module and generate its fields.
	if sel := r.URL.Query().Get("module"); sel != "" && s.Features.Registry {
		parts := strings.Split(sel, "/")
		if len(parts) != 4 {
			http.Error(w, "module must be namespace/name/system/version", http.StatusBadRequest)
			return
		}
		t, skipped, err := s.App.ModuleTemplate(r.Context(), parts[0], parts[1], parts[2], parts[3])
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		p.T.Module, p.T.Inputs = t.Module, t.Inputs
		if p.T.Name == "" {
			p.T.Name = t.Name
		}
		p.Selected = sel
		if len(skipped) > 0 {
			p.Warning = "Not generated (complex types need a default in the module or a future JSON field): " + strings.Join(skipped, ", ")
		}
	}
	s.renderBuilder(w, r, http.StatusOK, p)
}

func (s *Server) saveTemplate(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	t := templateFromForm(f)
	t.ID = f.Get("id") // posted by this page's earlier save
	editing := r.PathValue("id") != ""
	if editing {
		t.ID = r.PathValue("id")
	}
	page := builderPage{Editing: editing, Selected: f.Get("selected")}
	if f.Has("preview") { // the deploy form as end users get it, without saving
		rt, err := s.App.PreviewTemplate(r.Context(), actor(r), t, editing)
		if err != nil {
			page.T = rt
			s.builderError(w, r, page, err)
			return
		}
		vals, _ := app.FormDefaults(rt, nil)
		s.render(w, r, http.StatusOK, "form.html", &formPage{Page: Page{Section: "templates"}, T: rt, Values: vals, Preview: true})
		return
	}
	t, pr, err := s.App.SaveCatalogTemplate(r.Context(), actor(r), t, editing)
	page.T = t
	if err != nil {
		s.builderError(w, r, page, err)
		return
	}
	if pr != "" { // the catalog changes once it's merged
		page.PullRequest = pr
		s.renderBuilder(w, r, http.StatusOK, page)
		return
	}
	http.Redirect(w, r, "/templates/"+t.ID, http.StatusSeeOther)
}

// builderError shows a refused save on the builder form: invalid templates (422), conflicts (409),
// and the directory's or the Git provider's refusal (422, as these say). Not found and forbidden are
// answered as such.
func (s *Server) builderError(w http.ResponseWriter, r *http.Request, p builderPage, err error) {
	if errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
		s.uiError(w, r, err)
		return
	}
	f, status, ok := refused("template", r.PostForm, err)
	if !ok {
		s.App.Log.Warn("save template", "id", p.T.ID, "err", err)
		f, status = &formState{Name: "template", Message: err.Error()}, http.StatusUnprocessableEntity
	}
	p.F = f
	s.renderBuilder(w, r, status, p)
}

// deleteTemplate removes a template from the catalog (app.DeleteCatalogTemplate).
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	t, pr, err := s.App.DeleteCatalogTemplate(r.Context(), actor(r), r.PathValue("id"))
	page := builderPage{T: t, Editing: true}
	if err != nil {
		s.builderError(w, r, page, err)
		return
	}
	if pr != "" { // the template goes once it's merged
		page.PullRequest = pr
		s.renderBuilder(w, r, http.StatusOK, page)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// templateFromForm reads the builder form. Field cards are parallel in_* lists, rows without a name are
// dropped; the in_required/in_sensitive/in_per_environment checkboxes post their card's index, since unchecked ones post nothing.
// The key select posts the key input's name. Defaults: in_bool_default for Yes / no cards, in_default for the rest.
func templateFromForm(f url.Values) templates.Template {
	get := func(key string) string { return strings.TrimSpace(f.Get(key)) }
	t := templates.Template{
		Name: get("name"), Description: get("description"),
		Module: templates.Module{Source: get("module_source"), Ref: get("module_ref")},
	}
	at := func(key string, i int) string {
		if v := f[key]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	for i, name := range f["in_name"] {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		idx, typ := strconv.Itoa(i), at("in_type", i)
		def := at("in_default", i)
		if typ == "bool" {
			def = at("in_bool_default", i)
		}
		in := templates.Input{Name: name, Label: at("in_label", i), Help: at("in_help", i), Type: typ, Default: def,
			Required: typ != "bool" && typ != "select" && slices.Contains(f["in_required"], idx), Key: name == get("key"), // a dropdown always has a value
			PerEnvironment: slices.Contains(f["in_per_environment"], idx)}
		// the card hides the settings its type doesn't use; drop what they still post
		if typ == "string" {
			in.Pattern = at("in_pattern", i)
		}
		if typ == "string" || typ == "number" {
			in.Sensitive = slices.Contains(f["in_sensitive"], idx)
		}
		for j := range f["in_opt_value_"+idx] { // option rows post per card, named by its index like the checkboxes
			if value, label := at("in_opt_value_"+idx, j), at("in_opt_label_"+idx, j); value != "" && typ == "select" {
				if label == value {
					label = ""
				}
				in.Options = append(in.Options, templates.Option{Label: label, Value: value})
			}
		}
		if src := at("in_show_input", i); src != "" {
			in.ShowIf = &templates.ShowIf{Input: src}
			for _, v := range strings.Split(at("in_show_equals", i), ",") {
				if v = strings.TrimSpace(v); v != "" {
					in.ShowIf.Equals = append(in.ShowIf.Equals, v)
				}
			}
		}
		t.Inputs = append(t.Inputs, in)
	}
	return t
}
