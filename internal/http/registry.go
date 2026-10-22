package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-config-inspect/tfconfig"

	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/storage"
)

// Terraform registry protocol (modules.v1): https://developer.hashicorp.com/terraform/internals/module-registry-protocol
func (s *Server) registryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"modules.v1": "/v1/modules/"})
	})
	mux.Handle("GET /v1/modules/{ns}/{name}/{system}/versions", s.requireToken(storage.ScopeRegistry, s.moduleVersions))
	mux.Handle("GET /v1/modules/{ns}/{name}/{system}/{version}/download", s.requireToken(storage.ScopeRegistry, s.moduleDownload))
	mux.HandleFunc("GET /v1/archives/{ns}/{name}/{system}/{file}", s.moduleArchive)
	mux.HandleFunc("GET /registry", s.registryPage)
	mux.HandleFunc("GET /registry/{ns}/{name}/{system}/{version}", s.moduleDocs)
	mux.HandleFunc("GET /registry/{ns}/{name}/{system}/{version}/docs.md", s.moduleDocs)
	if s.Editor {
		mux.HandleFunc("POST /registry", s.admin(s.uploadModule))
	}
}

// requireToken checks a bearer token of the given scope (Terraform sends TF_TOKEN_<host> as one).
func (s *Server) requireToken(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok {
			valid, err := s.Store.ValidToken(r.Context(), token, scope)
			if err != nil {
				s.Log.Error("check token", "err", err)
				writeErrors(w, http.StatusInternalServerError, "internal error")
				return
			}
			ok = valid
		}
		if !ok {
			writeErrors(w, http.StatusUnauthorized, "missing or invalid API token")
			return
		}
		next(w, r)
	})
}

func (s *Server) moduleVersions(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Store.ModuleVersions(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"))
	if err != nil {
		s.Log.Error("module versions", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(vs) == 0 {
		writeErrors(w, http.StatusNotFound, "module not found")
		return
	}
	registry.SortVersions(vs)
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
	ns, name, sys, ver := r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), r.PathValue("version")
	if _, err := s.Store.GetModule(r.Context(), ns, name, sys, ver); errors.Is(err, sql.ErrNoRows) {
		writeErrors(w, http.StatusNotFound, "module version not found")
		return
	} else if err != nil {
		s.Log.Error("get module", "err", err)
		writeErrors(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Relative URL: Terraform resolves it against this endpoint's URL.
	w.Header().Set("X-Terraform-Get", s.Registry.SignedArchiveURL(registry.ArchivePath(ns, name, sys, ver), 10*time.Minute))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) moduleArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ver, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
	if !ok || !s.Registry.VerifyArchiveURL(r.URL.Path, q.Get("expires"), q.Get("sig")) {
		http.Error(w, "invalid or expired download link", http.StatusForbidden)
		return
	}
	b, err := s.Store.ModuleArchive(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), ver)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("module archive", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Write(b)
}

type registryPage struct {
	Modules []storage.Module
	Editor  bool
	Host    string
	Error   string
	Form    map[string]string
}

func (s *Server) registryPage(w http.ResponseWriter, r *http.Request) {
	s.renderRegistry(w, r, http.StatusOK, "", nil)
}

func (s *Server) renderRegistry(w http.ResponseWriter, r *http.Request, status int, errMsg string, form map[string]string) {
	mods, err := s.Store.ListModules(r.Context())
	if err != nil {
		s.Log.Error("list modules", "err", err)
		http.Error(w, "could not load modules", http.StatusInternalServerError)
		return
	}
	s.render(w, r, status, "registry.html", registryPage{Modules: mods, Editor: s.canEdit(r),
		Host: s.Registry.PublicURL.Host, Error: errMsg, Form: form})
}

func (s *Server) uploadModule(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, registry.MaxArchiveSize+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.renderRegistry(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("upload too large or malformed (max %d MB)", registry.MaxArchiveSize>>20), nil)
		return
	}
	defer r.MultipartForm.RemoveAll()
	form := map[string]string{}
	for _, k := range []string{"namespace", "name", "system", "version"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	fail := func(msg string) { s.renderRegistry(w, r, http.StatusUnprocessableEntity, msg, form) }

	ns, name, sys, _, err := s.Registry.Address(form["namespace"], form["name"], form["system"])
	if err != nil {
		fail(err.Error())
		return
	}
	ver, err := registry.CanonicalVersion(form["version"])
	if err != nil {
		fail(err.Error())
		return
	}
	f, _, err := r.FormFile("archive")
	if err != nil {
		fail("choose a .tar.gz module archive")
		return
	}
	defer f.Close()
	archive, err := io.ReadAll(io.LimitReader(f, registry.MaxArchiveSize+1))
	if err != nil {
		fail("could not read upload")
		return
	}
	if len(archive) > registry.MaxArchiveSize {
		fail(fmt.Sprintf("archive larger than %d MB", registry.MaxArchiveSize>>20))
		return
	}
	ins, err := registry.Inspect(archive)
	if err != nil {
		fail(err.Error())
		return
	}
	err = s.Store.CreateModule(r.Context(), storage.Module{Namespace: ns, Name: name, System: sys, Version: ver, Variables: ins.Variables},
		storage.ModuleDocs{Module: ins.Module, Readme: ins.Readme}, archive)
	if errors.Is(err, storage.ErrExists) {
		fail(fmt.Sprintf("%s/%s/%s %s already exists; versions are immutable, upload a new version", ns, name, sys, ver))
		return
	}
	if err != nil {
		s.Log.Error("create module", "err", err)
		fail("could not store module")
		return
	}
	s.Log.Info("module uploaded", "module", ns+"/"+name+"/"+sys, "version", ver, "variables", len(ins.Variables))
	http.Redirect(w, r, fmt.Sprintf("/registry/%s/%s/%s/%s", ns, name, sys, ver), http.StatusSeeOther)
}

// moduleDocs renders generated documentation for one module version, as HTML or (docs.md) Markdown.
func (s *Server) moduleDocs(w http.ResponseWriter, r *http.Request) {
	ns, name, sys, ver := r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"), r.PathValue("version")
	docs, err := s.Store.GetModuleDocs(r.Context(), ns, name, sys, ver)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("module docs", "err", err)
		http.Error(w, "could not load module docs", http.StatusInternalServerError)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/docs.md") {
		if docs.Module == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		docs.Module.Path = ns + "/" + name + "/" + sys + " " + ver // RenderMarkdown's title
		if err := tfconfig.RenderMarkdown(w, docs.Module); err != nil {
			s.Log.Error("render markdown", "err", err)
		}
		return
	}
	versions, err := s.Store.ModuleVersions(r.Context(), ns, name, sys)
	if err != nil {
		s.Log.Error("module versions", "err", err)
		http.Error(w, "could not load module", http.StatusInternalServerError)
		return
	}
	registry.SortVersions(versions)
	_, _, _, source, err := s.Registry.Address(ns, name, sys)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, http.StatusOK, "module.html", map[string]any{
		"Namespace": ns, "Name": name, "System": sys, "Version": ver, "Versions": versions,
		"Source": source, "Docs": docs, "Editor": s.canEdit(r),
	})
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
