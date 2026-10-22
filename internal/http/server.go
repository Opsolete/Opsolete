// Package httpserver contains HTTP handlers, routing and middleware.
package httpserver

import (
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
	"slices"
	"strings"
	"time"

	"gocloud.dev/blob"

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/templates"
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
	Log        *slog.Logger
	pages      *template.Template
}

// Features are the components this instance serves; a disabled one has no routes at all.
type Features struct {
	Deployments bool // templates, deploy forms, deployments, runner API
	Registry    bool // module registry pages and the Terraform registry protocol
	Editor      bool // template builder (needs Deployments) and module uploads (needs Registry), admins only
}

func New(store *storage.Store, tpls *templates.Catalog, reg *registry.Registry, a *auth.Auth, state *blob.Bucket, f Features, log *slog.Logger) *Server {
	pages := template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join, "json": toJSON,
		"user": func() auth.User { return auth.User{} }, "section": func() string { return "" },
		"features": func() Features { return f },
		"provider": func(p string) string { return strings.TrimPrefix(p, "registry.terraform.io/") },
		"status":   func(s string) string { return strings.ReplaceAll(s, "_", " ") }}).ParseFS(views, "views/*.html"))
	return &Server{Store: store, Templates: tpls, Registry: reg, Auth: a, StateStore: state, Features: f, Log: log, pages: pages}
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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	s.Auth.Routes(mux)
	if s.Features.Registry {
		s.registryRoutes(mux)
	}
	if !s.Features.Deployments {
		mux.Handle("GET /{$}", http.RedirectHandler("/registry", http.StatusFound))
		return s.logRequests(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))
	}
	mux.HandleFunc("GET /{$}", s.index)
	if s.Features.Editor {
		mux.HandleFunc("GET /templates/new", s.admin(s.builder))
		mux.HandleFunc("POST /templates/new", s.admin(s.saveTemplate))
		mux.HandleFunc("GET /templates/{id}/edit", s.admin(s.builder))
		mux.HandleFunc("POST /templates/{id}/edit", s.admin(s.saveTemplate))
	}
	s.runnerRoutes(mux)
	s.stateRoutes(mux)
	s.workspaceRoutes(mux)
	// CrossOriginProtection rejects cross-site browser POSTs (CSRF); token clients send no Origin and pass.
	return s.logRequests(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))
}

// public paths authenticate themselves (API tokens, signed URLs) or must work without login.
func public(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/.well-known/terraform.json" ||
		strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, runnerapi.Prefix+"/") ||
		strings.HasPrefix(path, runnerapi.StatePrefix+"/") || strings.HasPrefix(path, "/auth/")
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

// canEdit: the builder and module uploads are on and the user is an admin.
func (s *Server) canEdit(r *http.Request) bool {
	return s.Features.Editor && auth.FromContext(r.Context()).Admin()
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
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
	s.render(w, r, http.StatusOK, "index.html", map[string]any{"Templates": s.Templates.List(), "Editor": s.canEdit(r)})
}

type builderPage struct {
	T        templates.Template
	Rows     []templates.Input // existing inputs plus blank rows to fill in
	Editing  bool
	Error    string
	Warning  string
	Modules  []storage.Module // registry modules to start from
	Selected string           // ns/name/system/version picked from the registry
	Registry bool             // registry enabled: offer "start from a registry module"
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
	p.Rows = append(slices.Clone(p.T.Inputs), make([]templates.Input, 3)...)
	s.render(w, r, status, "builder.html", p)
}

func (s *Server) builder(w http.ResponseWriter, r *http.Request) {
	p := builderPage{T: templates.Template{Module: templates.Module{Ref: "v1.0.0"}}}
	if id := r.PathValue("id"); id != "" {
		t, ok := s.Templates.Get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		p.T, p.Editing = t, true
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
	if err := s.Templates.Save(t, editing); err != nil {
		fail(err.Error())
		return
	}
	s.Log.Info("template saved", "id", t.ID, "dir", s.Templates.Dir)
	http.Redirect(w, r, "/templates/"+t.ID, http.StatusSeeOther)
}

// toJSON renders a value (e.g. a variable default) compactly for display.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// newID returns a random UUIDv4, the same format as deployment ids.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// templateFromForm reads the builder form; input rows are parallel in_* lists and rows without a name are dropped.
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
		in := templates.Input{Name: name, Label: at("in_label", i), Type: at("in_type", i),
			Required: at("in_required", i) == "yes", Pattern: at("in_pattern", i), Default: at("in_default", i)}
		for _, o := range strings.Split(at("in_options", i), ",") {
			if o = strings.TrimSpace(o); o != "" {
				in.Options = append(in.Options, o)
			}
		}
		t.Inputs = append(t.Inputs, in)
	}
	return t
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	u := auth.FromContext(r.Context())
	pages, err := s.pages.Clone() // per request, so the layout can show the signed-in user
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// section is the first path segment, for highlighting the current nav entry.
	section, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	pages.Funcs(template.FuncMap{"user": func() auth.User { return u }, "section": func() string { return section }})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		s.Log.Error("render", "page", name, "err", err)
	}
}
