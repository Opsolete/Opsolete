// Package httpserver contains HTTP handlers, routing and middleware.
package httpserver

import (
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

//go:embed views/*.html
var views embed.FS

type Server struct {
	App      *app.App // the operations
	Auth     *auth.Auth
	Features Features
	// Organization is the one organization of the cloud API (Terraform's cloud block).
	Organization string
	RunnerToken  string             // RUNNER_TOKEN: the runner API's bearer token; empty rejects every runner
	pages        *template.Template // never executed: the base the per-request clones come from
	clones       sync.Pool          // of executed clones of pages: html/template escapes a clone once, not per request
	patterns     []string           // registered by Handler: access_test.go needs a matrix row for each
}

// routes is the server's mux; it records every pattern it registers in *patterns.
type routes struct {
	*http.ServeMux
	patterns *[]string
}

func (m routes) Handle(pattern string, h http.Handler) {
	*m.patterns = append(*m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

func (m routes) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

// Features are the components this instance serves; a disabled one has no routes at all.
type Features struct {
	Deployments bool // templates, deploy forms, deployments, runner API
	Registry    bool // module registry pages and the Terraform registry protocol
	// Editor (TEMPLATE_EDITOR, for development): the template builder writes into TEMPLATES_DIR,
	// and module uploads are on. A catalog in Git has its own editor mode, uploads their own setting.
	Editor bool
	VCS    bool // GitHub / Azure DevOps connections: publish registry modules (with Registry), link workspaces and the template catalog (with Deployments)
}

// New serves a's operations; a.RegistryEnabled, a.VCSEnabled and a.TemplateEditor follow f.
func New(a *app.App, au *auth.Auth, f Features) *Server {
	a.RegistryEnabled, a.VCSEnabled, a.TemplateEditor = f.Registry, f.VCS, f.Editor
	pages := template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join, "json": toJSON, "dict": dict,
		"user": func() auth.User { return auth.User{} }, "section": func() string { return "" },
		"workspace": func() string { return "" }, "project": func() string { return "" }, "tab": func() string { return "" },
		"features": func() Features { return f }, "duration": humanDuration, "ago": ago,
		"provider": func(p string) string { return strings.TrimPrefix(p, "registry.terraform.io/") },
		"status":   func(s string) string { return strings.ReplaceAll(s, "_", " ") }}).ParseFS(views, "views/*.html"))
	s := &Server{App: a, Auth: au, Features: f, pages: pages}
	s.clones.New = func() any { return template.Must(s.pages.Clone()) }
	return s
}

func (s *Server) Handler() http.Handler {
	s.patterns = nil
	mux := routes{http.NewServeMux(), &s.patterns}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	s.Auth.Routes(mux)
	mux.HandleFunc("GET /audit", s.admin(s.auditPage))
	mux.HandleFunc("GET /.well-known/terraform.json", s.discovery)
	if s.Features.Registry {
		s.registryRoutes(mux)
	}
	if s.Features.VCS {
		s.vcsRoutes(mux)
	}
	s.accountRoutes(mux)
	mux.HandleFunc("GET /organization", s.admin(s.organizationPage))
	mux.HandleFunc("POST /organization", s.admin(s.saveOrganization))
	if !s.Features.Deployments {
		mux.Handle("GET /{$}", http.RedirectHandler("/registry", http.StatusFound))
		return instrument(mux.ServeMux, s.logRequests(s.audit(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))))
	}
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /templates/new", s.admin(s.editor(s.builder)))
	mux.HandleFunc("POST /templates/new", s.admin(s.editor(s.saveTemplate)))
	mux.HandleFunc("GET /templates/{id}/edit", s.admin(s.editor(s.builder)))
	mux.HandleFunc("POST /templates/{id}/edit", s.admin(s.editor(s.saveTemplate)))
	mux.HandleFunc("POST /templates/{id}/delete", s.admin(s.editor(s.deleteTemplate)))
	mux.HandleFunc("GET /templates/{id}/rollout", s.admin(s.rolloutPage))
	mux.HandleFunc("POST /templates/{id}/rollout", s.admin(s.startRollout))
	mux.HandleFunc("POST /templates/{id}/rollout/{action}", s.admin(s.rolloutAction))
	if s.Features.VCS {
		mux.HandleFunc("GET /catalog", s.admin(s.catalogPage))
		mux.HandleFunc("POST /catalog", s.admin(s.saveCatalog))
		mux.HandleFunc("POST /catalog/sync", s.admin(s.syncCatalog))
		mux.HandleFunc("POST /catalog/delete", s.admin(s.deleteCatalog))
	}
	s.runnerRoutes(mux)
	s.stateRoutes(mux)
	s.cloudRoutes(mux)
	s.workspaceRoutes(mux)
	s.projectRoutes(mux)
	s.variableRoutes(mux)
	// CrossOriginProtection rejects cross-site browser POSTs (CSRF); token clients send no Origin and pass.
	return instrument(mux.ServeMux, s.logRequests(s.audit(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))))
}

// public paths authenticate themselves (API tokens, signed URLs) or must work without login.
func public(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/.well-known/terraform.json" ||
		strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, runnerapi.Prefix+"/") ||
		strings.HasPrefix(path, runnerapi.StatePrefix+"/") || strings.HasPrefix(path, "/auth/") ||
		strings.HasPrefix(path, cloudPrefix+"/") || path == "/oauth/token"
}

// requireLogin puts the signed-in user on the request; everything not public needs one.
func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := s.Auth.User(r)
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.FromContext(r.Context()).Admin() {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// canEditTemplates: deployments are on and app.CanEditTemplates.
func (s *Server) canEditTemplates(r *http.Request) bool {
	return s.Features.Deployments && s.App.CanEditTemplates(actor(r))
}

// editor answers 404 while the template builder is off.
func (s *Server) editor(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.canEditTemplates(r) {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// canUpload: the registry is on and app.CanUpload.
func (s *Server) canUpload(r *http.Request) bool {
	if !s.Features.Registry {
		return false
	}
	on, err := s.App.CanUpload(r.Context(), actor(r))
	if err != nil {
		s.App.Log.Error("module uploads setting", "err", err)
	}
	return on
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// no framing by other sites: confirm, destroy and authorize are one click (clickjacking)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
		s.App.Log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

// parseID parses an id from a request (path, form or JSON body). An input that isn't an id is
// treated as an unknown one: ok is false and the id uuid.Nil, which no row has.
func parseID(s string) (id uuid.UUID, ok bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false // Parse may return a partly filled id
	}
	return id, true
}

// formID is an id as a form value: "" for none (uuid.Nil).
func formID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// pathID parses the id in path segment name, as parseID.
func pathID(r *http.Request, name string) (uuid.UUID, bool) { return parseID(r.PathValue(name)) }

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Ready(r.Context()); err != nil {
		s.App.Log.Error("readiness check failed", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready"))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "index.html", map[string]any{"Templates": s.App.Templates.List(), "Editor": s.canEditTemplates(r)})
}

type builderPage struct {
	T           templates.Template
	Rows        []templates.Input // existing inputs plus blank rows to fill in
	Editing     bool
	Error       string
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
	s.render(w, r, status, "builder.html", p)
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	t := templateFromForm(r)
	t.ID = r.PostForm.Get("id") // posted by this page's earlier save
	editing := r.PathValue("id") != ""
	if editing {
		t.ID = r.PathValue("id")
	}
	page := builderPage{Editing: editing, Selected: r.PostForm.Get("selected")}
	if r.PostForm.Has("preview") { // the deploy form as end users get it, without saving
		rt, err := s.App.PreviewTemplate(r.Context(), actor(r), t, editing)
		if err != nil {
			page.T = rt
			s.builderError(w, r, page, err)
			return
		}
		vals, _ := app.FormDefaults(rt, nil)
		s.render(w, r, http.StatusOK, "form.html", formPage{T: rt, Values: vals, Preview: true})
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

// builderError shows a refused save on the builder form: invalid templates, and the directory's or
// the Git provider's refusal (as these say). Not found and forbidden are answered as such.
func (s *Server) builderError(w http.ResponseWriter, r *http.Request, p builderPage, err error) {
	if errors.Is(err, app.ErrNotFound) || errors.Is(err, app.ErrForbidden) {
		s.uiError(w, r, err)
		return
	}
	if !isValidation(err) {
		s.App.Log.Warn("save template", "id", p.T.ID, "err", err)
	}
	p.Error = err.Error()
	s.renderBuilder(w, r, http.StatusUnprocessableEntity, p)
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

// toJSON renders a value (e.g. a variable default) compactly for display.
// dict makes a map of key, value pairs, to pass several values to a shared template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, errors.New("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v isn't a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// templateFromForm reads the builder form. Field cards are parallel in_* lists, rows without a name are
// dropped; the in_required/in_sensitive checkboxes post their card's index, since unchecked ones post nothing.
// The key select posts the key input's name.
func templateFromForm(r *http.Request) templates.Template {
	f := r.PostForm
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
		in := templates.Input{Name: name, Label: at("in_label", i), Type: typ, Default: at("in_default", i),
			Required: typ != "bool" && typ != "select" && slices.Contains(f["in_required"], idx), Key: name == get("key")} // a dropdown always has a value
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

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	u := auth.FromContext(r.Context())
	pages := s.clones.Get().(*template.Template) // one request at a time per clone, so its funcs can show this request's user
	defer s.clones.Put(pages)
	// section is the first path segment, for highlighting the current nav entry.
	section, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	// inside a workspace or project the side nav is its menu; tab is the page within it
	workspace, project := r.PathValue("name"), r.PathValue("project")
	if section != "workspaces" {
		workspace = ""
	}
	if section != "projects" {
		project = ""
	}
	_, tab, _ := strings.Cut(rest, "/")
	tab, _, _ = strings.Cut(tab, "/")
	if tab == "" {
		tab = "overview"
	}
	pages.Funcs(template.FuncMap{"user": func() auth.User { return u }, "section": func() string { return section },
		"workspace": func() string { return workspace }, "project": func() string { return project }, "tab": func() string { return tab }})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		s.App.Log.Error("render", "page", name, "err", err)
	}
}
