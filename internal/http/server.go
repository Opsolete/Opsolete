// Package httpserver contains HTTP handlers, routing and middleware.
package httpserver

import (
	"bytes"
	"context"
	"embed"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

//go:embed views/*.html
var views embed.FS

type Server struct {
	App      *app.App // the operations
	Auth     *auth.Auth
	Features Features
	// Organization is the one organization of the cloud API (Terraform's cloud block).
	Organization string
	ClaimWait    time.Duration      // how long a runner's claim waits for a job: below typical proxy idle timeouts
	pages        *template.Template // views/*.html, parsed once; render executes them
	patterns     []string           // registered by Handler: access_test.go needs a matrix row for each
	streams      struct {           // open log streams per user (logStream)
		sync.Mutex
		n map[uuid.UUID]int
	}
	// Stopping ends with shutdown. Shutdown waits for requests but doesn't cancel them: this ends the
	// long-lived ones (log streams, runner claims), while the rest (state writes, finish) drain.
	Stopping context.Context
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
		"duration": humanDuration, "ago": ago, "asset": asset, "confirmWhy": confirmWhy,
		"provider": func(p string) string { return strings.TrimPrefix(p, "registry.terraform.io/") },
		"status":   func(s string) string { return strings.ReplaceAll(s, "_", " ") }}).Funcs(formFuncs).ParseFS(views, "views/*.html"))
	return &Server{App: a, Auth: au, Features: f, ClaimWait: 20 * time.Second, Stopping: context.Background(), pages: pages}
}

func (s *Server) Handler() http.Handler {
	s.patterns = nil
	mux := routes{http.NewServeMux(), &s.patterns}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /static/{file}", static)
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
	if s.Features.Deployments {
		mux.HandleFunc("GET /organization/runners", s.admin(s.runnersPage))
		mux.HandleFunc("POST /organization/runners", s.admin(s.createRunner))
		mux.HandleFunc("POST /organization/runners/{id}/delete", s.admin(s.deleteRunner))
	}
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
	s.apiRoutes(mux)
	// CrossOriginProtection rejects cross-site browser POSTs (CSRF); token clients send no Origin and pass.
	return instrument(mux.ServeMux, s.logRequests(s.audit(http.NewCrossOriginProtection().Handler(s.requireLogin(mux)))))
}

// public paths authenticate themselves (API tokens, signed URLs) or must work without login.
func public(path string) bool {
	return path == "/healthz" || path == "/readyz" || path == "/.well-known/terraform.json" || strings.HasPrefix(path, "/static/") ||
		strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, runnerapi.Prefix+"/") ||
		strings.HasPrefix(path, runnerapi.StatePrefix+"/") || strings.HasPrefix(path, "/auth/") ||
		strings.HasPrefix(path, cloudPrefix+"/") || path == "/oauth/token" || strings.HasPrefix(path, apiPrefix+"/")
}

// requireLogin puts the signed-in user on the request; everything not public needs one.
func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.CurrentPrincipal(r.Context()) // looked up by audit
		if p.Type != "user" {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), p.User)))
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
		w.Header().Set("Content-Security-Policy", csp("'self'"))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
		s.App.Log.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

// csp is the Content-Security-Policy of every response: scripts and styles only from /static
// (no inline ones), forms posting to formAction (a POST's redirect counts too).
func csp(formAction string) string {
	return "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"form-action " + formAction + "; frame-ancestors 'none'; base-uri 'none'"
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

// Page is the part of every page model the layout shows: the top bar and the side nav. Each model
// embeds it; its handler says where the page is (Section, and inside a workspace or project its menu),
// render fills in User and Features.
type Page struct {
	User     auth.User
	Features Features
	Section  string // the main nav entry: "templates", "workspaces", "projects", "registry", "account", "vcs", …
	// inside a workspace or a project the side nav is its menu
	InWorkspace, InProject string // its name
	Tab                    string // the menu entry: "overview", "runs", "settings", "vcs", …
	Sub                    string // the settings (or account) sub-page; "" is General
	CanOperate             bool   // in a workspace: the user operates it (its menu offers the settings)
	WorkspaceCount         int    // in a project: its workspaces, on its menu
}

func (p *Page) base() *Page { return p }

// workspaceNav is the Page of a workspace's menu page tab (sub: its settings sub-page).
func workspaceNav(name, tab, sub string, canOperate bool) Page {
	return Page{Section: "workspaces", InWorkspace: name, Tab: tab, Sub: sub, CanOperate: canOperate}
}

// render executes the page template name with data, a pointer to a page model (it fills in the
// model's User and Features), and only then writes it with status: a template error is a logged 500.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data interface{ base() *Page }) {
	p := data.base()
	p.User, p.Features = auth.FromContext(r.Context()), s.Features
	var b bytes.Buffer
	if err := s.pages.ExecuteTemplate(&b, name, data); err != nil {
		s.App.Log.Error(errRender, "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	b.WriteTo(w)
}

// errRender is logged for a template that failed to execute (tests fail on it).
const errRender = "render page"
