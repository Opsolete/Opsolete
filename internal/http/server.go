// Package httpserver contains HTTP handlers, routing and middleware.
package httpserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gocloud.dev/blob"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

//go:embed views/*.html
var views embed.FS

type Server struct {
	Store     *storage.Store
	Templates *templates.Catalog
	Registry  *registry.Registry
	Auth      *auth.Auth
	// StateStore holds the workspaces' Terraform state files (any gocloud.dev/blob URL).
	StateStore *blob.Bucket
	Features   Features
	SecretKey  []byte // from SECRET_KEY (required): encrypts VCS tokens, variables and state store files
	// Organization is the one organization of the cloud API (Terraform's cloud block).
	Organization string
	RunnerToken  string // RUNNER_TOKEN: the runner API's bearer token; empty rejects every runner
	Log          *slog.Logger
	pages        *template.Template // never executed: the base the per-request clones come from
	clones       sync.Pool          // of executed clones of pages: html/template escapes a clone once, not per request
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

func New(store *storage.Store, tpls *templates.Catalog, reg *registry.Registry, a *auth.Auth, state *blob.Bucket, f Features, log *slog.Logger) *Server {
	pages := template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join, "json": toJSON,
		"user": func() auth.User { return auth.User{} }, "section": func() string { return "" },
		"workspace": func() string { return "" }, "project": func() string { return "" }, "tab": func() string { return "" },
		"features": func() Features { return f }, "duration": humanDuration, "ago": ago,
		"provider": func(p string) string { return strings.TrimPrefix(p, "registry.terraform.io/") },
		"status":   func(s string) string { return strings.ReplaceAll(s, "_", " ") }}).ParseFS(views, "views/*.html"))
	s := &Server{Store: store, Templates: tpls, Registry: reg, Auth: a, StateStore: state, Features: f, Log: log, pages: pages}
	s.clones.New = func() any { return template.Must(s.pages.Clone()) }
	return s
}

// CheckTemplates fails if a template needs a disabled component: with the registry off,
// a template pointing at it would only fail later, on the runner.
func (s *Server) CheckTemplates() error {
	for _, t := range s.Templates.List() {
		if err := s.checkSource(t); err != nil {
			return fmt.Errorf("template %s: %w", t.ID, err)
		}
	}
	return nil
}

func (s *Server) checkSource(t templates.Template) error {
	if _, _, _, ok := s.Registry.ParseSource(t.Module.Source); ok && !s.Features.Registry {
		return fmt.Errorf("module %s is in this registry, which is disabled (REGISTRY_ENABLED=false)", t.Module.Source)
	}
	return nil
}

// template returns template id with its inputs resolved from the variables of the registry module
// version it pins (registry.Resolve), so the form can't drift from the module's variables.tf.
func (s *Server) template(ctx context.Context, id string) (templates.Template, bool, error) {
	t, ok := s.Templates.Get(id)
	if !ok {
		return t, false, nil
	}
	t, err := s.resolve(ctx, t)
	return t, err == nil, err
}

// resolve returns t with its inputs resolved from the registry module version it pins, if any.
func (s *Server) resolve(ctx context.Context, t templates.Template) (templates.Template, error) {
	if !s.Features.Registry {
		return t, nil
	}
	// shortcut: git:: and other external sources keep their JSON inputs, since the server doesn't fetch modules. Publish them to the registry (e.g. via VCS) to get generated inputs.
	ns, name, sys, isReg := s.Registry.ParseSource(t.Module.Source)
	if !isReg {
		return t, nil
	}
	m, err := s.Store.GetModule(ctx, ns, name, sys, t.Module.Ref)
	if errors.Is(err, sql.ErrNoRows) { // saveTemplate refuses this; a hand-written JSON fails at terraform init
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.Inputs = registry.Resolve(t.Inputs, m.Variables)
	return t, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
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
		return instrument(mux, s.logRequests(s.audit(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))))
	}
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /templates/new", s.admin(s.editor(s.builder)))
	mux.HandleFunc("POST /templates/new", s.admin(s.editor(s.saveTemplate)))
	mux.HandleFunc("GET /templates/{id}/edit", s.admin(s.editor(s.builder)))
	mux.HandleFunc("POST /templates/{id}/edit", s.admin(s.editor(s.saveTemplate)))
	mux.HandleFunc("POST /templates/{id}/delete", s.admin(s.editor(s.deleteTemplate)))
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
	return instrument(mux, s.logRequests(s.audit(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))))
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

// canEditTemplates: the user is an admin and the template builder is on (TEMPLATE_EDITOR for
// TEMPLATES_DIR, or the Git catalog's editor mode).
func (s *Server) canEditTemplates(r *http.Request) bool {
	if !s.Features.Deployments || !auth.FromContext(r.Context()).Admin() {
		return false
	}
	switch mode, _ := s.Templates.Source(); mode {
	case "":
		return s.Features.Editor
	case "commit", "pr":
		return true
	}
	return false
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

// canUpload: the user is an admin and module uploads are on (Organization settings, or TEMPLATE_EDITOR).
func (s *Server) canUpload(r *http.Request) bool {
	if !s.Features.Registry || !auth.FromContext(r.Context()).Admin() {
		return false
	}
	if s.Features.Editor {
		return true
	}
	on, err := s.Store.ModuleUploads(r.Context())
	if err != nil {
		s.Log.Error("module uploads setting", "err", err)
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
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DB.PingContext(r.Context()); err != nil {
		s.Log.Error("readiness check failed", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready"))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "index.html", map[string]any{"Templates": s.Templates.List(), "Editor": s.canEditTemplates(r)})
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
		mods, err := s.Store.ListModules(r.Context())
		if err != nil {
			s.Log.Error("list modules", "err", err)
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
		t, ok, err := s.template(r.Context(), id)
		if err != nil {
			s.internalError(w, "template", err)
			return
		}
		if !ok {
			http.NotFound(w, r)
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
		m, err := s.Store.GetModule(r.Context(), parts[0], parts[1], parts[2], parts[3])
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.Log.Error("get module", "err", err)
			http.Error(w, "could not load module", http.StatusInternalServerError)
			return
		}
		_, _, _, source, err := s.Registry.Address(m.Namespace, m.Name, m.System)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		inputs, skipped := registry.Inputs(m.Variables)
		p.T.Module = templates.Module{Source: source, Ref: m.Version}
		p.T.Inputs = inputs
		if p.T.Name == "" {
			p.T.Name = m.Name
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
	editing := r.PathValue("id") != ""
	if editing {
		if _, ok := s.Templates.Get(r.PathValue("id")); !ok {
			http.NotFound(w, r)
			return
		}
		t.ID = r.PathValue("id") // the id is the file name; it can't change on edit
	} else if id := r.PostForm.Get("id"); uuidRe.MatchString(id) { // posted by this page's earlier save
		t.ID = id
	} else {
		t.ID = newID()
	}
	fail := func(msg string) {
		s.renderBuilder(w, r, http.StatusUnprocessableEntity, builderPage{T: t, Editing: editing, Error: msg, Selected: r.PostForm.Get("selected")})
	}
	if err := s.checkSource(t); err != nil {
		fail(err.Error())
		return
	}
	// A template pointing at this registry must reference a version that exists, or the deploy fails later.
	if ns, name, sys, ok := s.Registry.ParseSource(t.Module.Source); ok {
		if _, err := s.Store.GetModule(r.Context(), ns, name, sys, t.Module.Ref); errors.Is(err, sql.ErrNoRows) {
			fail(fmt.Sprintf("registry module %s has no version %q", t.Module.Source, t.Module.Ref))
			return
		} else if err != nil {
			s.Log.Error("get module", "err", err)
			fail("could not check registry module")
			return
		}
	}
	if r.PostForm.Has("preview") { // the deploy form as end users get it, without saving
		if _, err := templates.Encode(t); err != nil {
			fail(err.Error())
			return
		}
		rt, err := s.resolve(r.Context(), t)
		if err != nil {
			s.internalError(w, "resolve template", err)
			return
		}
		vals, _ := formDefaults(rt, nil)
		s.render(w, r, http.StatusOK, "form.html", formPage{T: rt, Values: vals, Preview: true})
		return
	}
	if mode, _ := s.Templates.Source(); mode != "" {
		pr, err := vcs.SaveTemplate(r.Context(), s.Store, s.SecretKey, s.Templates, t, auth.FromContext(r.Context()).Email)
		if err != nil {
			s.Log.Warn("save template to git", "id", t.ID, "err", err)
			fail(err.Error())
			return
		}
		if pr != "" { // the catalog changes once it's merged
			s.Log.Info("template pull request opened", "id", t.ID, "url", pr)
			s.renderBuilder(w, r, http.StatusOK, builderPage{T: t, Editing: editing, PullRequest: pr, Selected: r.PostForm.Get("selected")})
			return
		}
	} else if err := s.Templates.Save(t, editing); err != nil {
		fail(err.Error())
		return
	}
	s.Log.Info("template saved", "id", t.ID)
	http.Redirect(w, r, "/templates/"+t.ID, http.StatusSeeOther)
}

// deleteTemplate removes a template from the catalog. Workspaces created from it keep running
// their pinned module; they just can't be reconfigured from it.
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Templates.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	fail := func(msg string) {
		s.renderBuilder(w, r, http.StatusUnprocessableEntity, builderPage{T: t, Editing: true, Error: msg})
	}
	if mode, _ := s.Templates.Source(); mode != "" {
		pr, err := vcs.DeleteTemplate(r.Context(), s.Store, s.SecretKey, s.Templates, t.ID, auth.FromContext(r.Context()).Email)
		if err != nil {
			s.Log.Warn("delete template in git", "id", t.ID, "err", err)
			fail(err.Error())
			return
		}
		if pr != "" { // the template goes once it's merged
			s.Log.Info("template removal pull request opened", "id", t.ID, "url", pr)
			s.renderBuilder(w, r, http.StatusOK, builderPage{T: t, Editing: true, PullRequest: pr})
			return
		}
	} else if err := s.Templates.Delete(t.ID); err != nil {
		fail(err.Error())
		return
	}
	s.Log.Info("template deleted", "id", t.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// toJSON renders a value (e.g. a variable default) compactly for display.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`) // newID's format

// newID returns a random UUIDv4, the same format as deployment ids.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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
		s.Log.Error("render", "page", name, "err", err)
	}
}
