package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
)

// Terraform registry protocol (modules.v1): https://developer.hashicorp.com/terraform/internals/module-registry-protocol
func (s *Server) registryRoutes(mux routes) {
	mux.Handle("GET /v1/modules/{ns}/{name}/{system}/versions", s.requireToken(storage.ScopeRegistry, s.moduleVersions))
	mux.Handle("GET /v1/modules/{ns}/{name}/{system}/{version}/download", s.requireToken(storage.ScopeRegistry, s.moduleDownload))
	mux.HandleFunc("GET /v1/archives/{ns}/{name}/{system}/{file}", s.moduleArchive)
	mux.HandleFunc("GET /registry", s.registryPage)
	mux.HandleFunc("GET /registry/{ns}/{name}/{system}/{version}", s.moduleDocs)
	mux.HandleFunc("GET /registry/{ns}/{name}/{system}/{version}/docs.md", s.moduleDocs)
	mux.HandleFunc("GET /registry/publish", s.admin(s.publishPage))
	mux.HandleFunc("POST /registry", s.admin(s.uploadModule))
	mux.HandleFunc("POST /registry/{ns}/{name}/{system}/delete", s.admin(s.deleteModule))
}

// deleteModule removes all versions of a module (app.DeleteModule).
func (s *Server) deleteModule(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteModule(r.Context(), actor(r), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/registry", http.StatusSeeOther)
}

// requireToken checks a bearer token of the given scope (Terraform sends TF_TOKEN_<host> as one).
func (s *Server) requireToken(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			var err error
			if ok, err = s.App.ValidToken(r.Context(), token, scope); err != nil {
				s.App.Log.Error("check token", "err", err)
				writeErrors(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
		if !ok {
			writeErrors(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next(w, r)
	})
}

func (s *Server) moduleVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.App.ModuleVersions(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"))
	if err != nil {
		s.runnerError(w, r, err)
		return
	}
	type ver struct {
		Version string `json:"version"`
	}
	list := make([]ver, len(vs))
	for i, v := range vs {
		list[i] = ver{v}
	}
	writeJSON(w, map[string]any{"modules": []any{map[string]any{"versions": list}}})
}

func (s *Server) moduleDownload(w http.ResponseWriter, r *http.Request) {
	u, err := s.App.ModuleDownload(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), r.PathValue("version"))
	if err != nil {
		s.runnerError(w, r, err)
		return
	}
	w.Header().Set("X-Terraform-Get", u)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) moduleArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ver := strings.TrimSuffix(r.PathValue("file"), ".tar.gz") // without it, the signature can't match
	b, err := s.App.ModuleArchive(r.Context(), r.URL.Path, q.Get("expires"), q.Get("sig"), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), ver)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Write(b)
}

// registryPage lists the registry's modules (registry.html).
type registryPage struct {
	Page
	Modules   []latestModule
	Templates bool // the template builder is on: offer "Create template"
	Upload    bool
	VCS       bool // VCS publishing is on and the user is an admin
	Admin     bool // may delete modules
	Linked    []storage.VCSModule
	Host      string
}

// latestModule is a module's newest version and how many older ones it has.
type latestModule struct {
	storage.Module
	Older int
}

// latestVersions keeps each module's highest semver version (not the latest upload: a backport
// isn't the newest version). mods is ordered by module.
func latestVersions(mods []storage.Module) []latestModule {
	var out []latestModule
	for _, m := range mods {
		n := len(out)
		if n == 0 || out[n-1].Namespace != m.Namespace || out[n-1].Name != m.Name || out[n-1].System != m.System {
			out = append(out, latestModule{Module: m})
			continue
		}
		out[n-1].Older++
		vs := []string{out[n-1].Version, m.Version}
		if registry.SortVersions(vs); vs[0] == m.Version {
			out[n-1].Module = m
		}
	}
	return out
}

func (s *Server) registryPage(w http.ResponseWriter, r *http.Request) {
	mods, err := s.App.Modules(r.Context())
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	p := &registryPage{Page: Page{Section: "registry"}, Modules: latestVersions(mods), Templates: s.canEditTemplates(r), Upload: s.canUpload(r), Host: s.App.Registry.PublicURL.Host,
		VCS: s.Features.VCS && auth.FromContext(r.Context()).Admin(), Admin: auth.FromContext(r.Context()).Admin()}
	if p.VCS {
		if p.Linked, err = s.App.VCSModules(r.Context(), actor(r)); err != nil {
			s.uiError(w, r, err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "registry.html", p)
}

// renderUpload is the upload step of the add-module wizard.
func (s *Server) renderUpload(w http.ResponseWriter, r *http.Request, status int, f *formState, form map[string]string) {
	s.render(w, r, status, "publish.html", &publishPage{Page: Page{Section: "registry"}, Type: "upload", Host: s.App.Registry.PublicURL.Host, F: f, Form: form})
}

func (s *Server) uploadModule(w http.ResponseWriter, r *http.Request) {
	if !s.canUpload(r) { // before reading the archive
		http.Error(w, "module uploads are off (Settings → Organization)", http.StatusForbidden)
		return
	}
	f, tooLarge, ok := readMultipart(w, r, registry.MaxArchiveSize+1<<20)
	if !ok {
		return
	}
	form := trimmed(f, "namespace", "name", "system", "version")
	if tooLarge {
		s.renderUpload(w, r, http.StatusRequestEntityTooLarge, &formState{Name: "upload", Input: f,
			Fields: map[string]string{"archive": fmt.Sprintf("archive larger than %d MB", registry.MaxArchiveSize>>20)}}, form)
		return
	}
	defer r.MultipartForm.RemoveAll()
	in := app.ModuleUpload{Namespace: form["namespace"], Name: form["name"], System: form["system"], Version: form["version"]}
	if file, _, err := r.FormFile("archive"); err == nil { // none chosen: app says so
		defer file.Close()
		if in.Archive, err = io.ReadAll(io.LimitReader(file, registry.MaxArchiveSize+1)); err != nil {
			s.renderUpload(w, r, http.StatusUnprocessableEntity, &formState{Name: "upload", Input: f, Fields: map[string]string{"archive": "could not read the upload"}}, form)
			return
		}
	}
	m, err := s.App.UploadModule(r.Context(), actor(r), in)
	if s.failed(w, r, err, "upload", f, func(status int, fs *formState) { s.renderUpload(w, r, status, fs, form) }) {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/registry/%s/%s/%s/%s", m.Namespace, m.Name, m.System, m.Version), http.StatusSeeOther)
}

// moduleDocs renders generated documentation for one module version, as HTML or (docs.md) Markdown.
func (s *Server) moduleDocs(w http.ResponseWriter, r *http.Request) {
	ns, name, sys, ver := r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), r.PathValue("version")
	d, err := s.App.ModuleDocs(r.Context(), ns, name, sys, ver)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/docs.md") {
		if d.Docs.Module == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		d.Docs.Module.Path = ns + "/" + name + "/" + sys + " " + ver // RenderMarkdown's title
		if err := tfconfig.RenderMarkdown(w, d.Docs.Module); err != nil {
			s.App.Log.Error("render markdown", "err", err)
		}
		return
	}
	if !s.Features.Deployments { // No-code ready: only with templates to deploy
		d.Templates = nil
	}
	can, err := s.canCreateWorkspaces(r)
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	s.render(w, r, http.StatusOK, "module.html", &modulePage{Page{Section: "registry"}, ns, name, sys, ver, d, s.canEditTemplates(r), can})
}

// modulePage is a module version's documentation (module.html).
type modulePage struct {
	Page
	Namespace, Name, System, Version string
	app.ModuleDocs
	Editor    bool // the template builder is on: offer "Create template"
	CanCreate bool // the user may create workspaces somewhere
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// writeErrors uses the registry API's error shape ({"errors": [...]}).
func writeErrors(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string][]string{"errors": {msg}})
}
