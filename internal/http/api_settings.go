package httpserver

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// The organization's settings a Terraform provider manages too (admins): runners, registry modules published from a
// repository, and the template catalog. Each handler is the UI's app function, as in api_platform.go.

func (s *Server) apiSettingsRoutes(mux routes) {
	p, u := apiPrefix, s.apiUser
	mux.HandleFunc("GET "+p+"/runners", u(s.apiRunners))
	mux.HandleFunc("POST "+p+"/runners", u(s.apiCreateRunner))
	mux.HandleFunc("GET "+p+"/runners/{id}", u(s.apiGetRunner))
	mux.HandleFunc("DELETE "+p+"/runners/{id}", u(s.apiDeleteRunner))
	mux.HandleFunc("GET "+p+"/vcs-modules", u(s.apiVCSModules))
	mux.HandleFunc("POST "+p+"/vcs-modules", u(s.apiPublishVCSModule))
	mux.HandleFunc("GET "+p+"/vcs-modules/{namespace}/{name}/{system}", u(s.apiGetVCSModule))
	mux.HandleFunc("DELETE "+p+"/vcs-modules/{namespace}/{name}/{system}", u(s.apiUnlinkVCSModule))
	mux.HandleFunc("GET "+p+"/template-catalog", u(s.apiTemplateCatalog))
	mux.HandleFunc("PUT "+p+"/template-catalog", u(s.apiSetTemplateCatalog))
	mux.HandleFunc("DELETE "+p+"/template-catalog", u(s.apiDisconnectCatalog))
}

// adminsOnly answers 403 for others: these objects are admins' (before any lookup, so an id says nothing).
func adminsOnly(w http.ResponseWriter, r *http.Request) bool {
	if !actor(r).Admin {
		writeAPIv1Error(w, http.StatusForbidden, "admins only", nil)
		return false
	}
	return true
}

// Runners.

type apiRunner struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Pool     string     `json:"pool"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Token    string     `json:"token,omitempty"` // only when created with a new one: shown once
}

func toAPIRunner(r storage.Runner) apiRunner {
	return apiRunner{ID: r.ID.String(), Name: r.Name, Pool: r.Pool, LastSeen: r.LastSeen}
}

// apiRunners lists the runners (?name= filters).
func (s *Server) apiRunners(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Runners(r.Context(), actor(r))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiRunner{}
	for _, rn := range list {
		if name := r.URL.Query().Get("name"); name == "" || rn.Name == name {
			out = append(out, toAPIRunner(rn))
		}
	}
	writeAPIv1(w, http.StatusOK, out)
}

// runnerBy finds a runner; ErrNotFound.
func (s *Server) runnerBy(r *http.Request, match func(storage.Runner) bool) (storage.Runner, error) {
	list, err := s.App.Runners(r.Context(), actor(r))
	if err != nil {
		return storage.Runner{}, err
	}
	if i := slices.IndexFunc(list, match); i >= 0 {
		return list[i], nil
	}
	return storage.Runner{}, app.ErrNotFound
}

// apiCreateRunner registers a runner in a pool (default "default"): with a new token, returned once (201, "token"), or
// with the token sent (from a secret store: at least 32 characters), which replaces that runner's token and pool.
func (s *Server) apiCreateRunner(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string `json:"name"`
		Pool  string `json:"pool"`
		Token string `json:"token"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	pool := orDefault(in.Pool, storage.DefaultPool)
	var token string
	var err error
	if in.Token != "" {
		err = s.App.RegisterRunner(r.Context(), actor(r), in.Name, pool, in.Token)
	} else {
		token, err = s.App.CreateRunner(r.Context(), actor(r), in.Name, pool)
	}
	var rn storage.Runner
	if err == nil {
		rn, err = s.runnerBy(r, func(x storage.Runner) bool { return x.Name == strings.TrimSpace(in.Name) })
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := toAPIRunner(rn)
	out.Token = token
	writeAPIv1(w, http.StatusCreated, out)
}

func (s *Server) apiGetRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(w, r, "id")
	if !ok {
		return
	}
	rn, err := s.runnerBy(r, func(x storage.Runner) bool { return x.ID == id })
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, toAPIRunner(rn))
}

// apiDeleteRunner deletes a runner: its token stops working at once. 204.
func (s *Server) apiDeleteRunner(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.App.DeleteRunner(r.Context(), actor(r), id); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Registry modules published from a repository, addressed by their registry address (which never changes).

type apiVCSModule struct {
	Namespace    string     `json:"namespace"`
	Name         string     `json:"name"`
	System       string     `json:"system"`
	ConnectionID string     `json:"connection_id"`
	Repo         string     `json:"repo"`
	Dir          string     `json:"dir"`
	TagPrefix    string     `json:"tag_prefix"`
	SyncedAt     *time.Time `json:"synced_at,omitempty"`
	SyncError    string     `json:"sync_error,omitempty"`
	Versions     []string   `json:"versions"`
}

func (s *Server) toAPIVCSModule(r *http.Request, m storage.VCSModule) (apiVCSModule, error) {
	vs, err := s.App.ModuleVersions(r.Context(), m.Namespace, m.Name, m.System)
	if vs == nil {
		vs = []string{}
	}
	return apiVCSModule{Namespace: m.Namespace, Name: m.Name, System: m.System, ConnectionID: m.ConnectionID.String(), Repo: m.Repo, Dir: m.Dir,
		TagPrefix: m.TagPrefix, SyncedAt: m.SyncedAt, SyncError: m.SyncError, Versions: vs}, err
}

func (s *Server) apiVCSModules(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.VCSModules(r.Context(), actor(r))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiVCSModule{}
	for _, m := range list {
		j, err := s.toAPIVCSModule(r, m)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiPublishVCSModule links a repository to a registry module and publishes its tags right away: 201 with the module
// and its versions.
func (s *Server) apiPublishVCSModule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConnectionID string `json:"connection_id"`
		Repo         string `json:"repo"`
		Dir          string `json:"dir"`
		TagPrefix    string `json:"tag_prefix"`
		Namespace    string `json:"namespace"`
		Name         string `json:"name"`
		System       string `json:"system"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cid, err := uuid.Parse(in.ConnectionID)
	if err != nil {
		s.v1Error(w, r, &app.ValidationError{Fields: map[string]string{"connection_id": "not a connection id"}})
		return
	}
	m, _, err := s.App.PublishVCSModule(r.Context(), actor(r), app.NewVCSModule{ConnectionID: cid, Repo: strings.TrimSpace(in.Repo),
		Dir: strings.TrimSpace(in.Dir), TagPrefix: strings.TrimSpace(in.TagPrefix), Namespace: strings.TrimSpace(in.Namespace),
		Name: strings.TrimSpace(in.Name), System: strings.TrimSpace(in.System)})
	var out apiVCSModule
	if err == nil {
		out, err = s.toAPIVCSModule(r, m)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusCreated, out)
}

func (s *Server) apiGetVCSModule(w http.ResponseWriter, r *http.Request) {
	if !adminsOnly(w, r) {
		return
	}
	m, err := s.App.VCSModule(r.Context(), r.PathValue("namespace"), r.PathValue("name"), r.PathValue("system"))
	if err == nil && m == nil {
		err = app.ErrNotFound // a module that isn't published from a repository
	}
	var out apiVCSModule
	if err == nil {
		out, err = s.toAPIVCSModule(r, *m)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiUnlinkVCSModule stops publishing the module from its repository; its published versions stay (workspaces pin
// them). 204.
func (s *Server) apiUnlinkVCSModule(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnlinkVCSModule(r.Context(), actor(r), r.PathValue("namespace"), r.PathValue("name"), r.PathValue("system")); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The template catalog: a folder of a repository branch, one per installation.

type apiTemplateCatalog struct {
	ConnectionID string     `json:"connection_id"`
	Repo         string     `json:"repo"`
	Branch       string     `json:"branch"`
	Dir          string     `json:"dir"`
	Mode         string     `json:"mode"` // what saving a template in the builder does: off, commit or pr
	Commit       string     `json:"commit,omitempty"`
	SyncedAt     *time.Time `json:"synced_at,omitempty"`
	SyncError    string     `json:"sync_error,omitempty"`
}

// apiTemplateCatalog is the catalog's settings; 404 while the templates come from TEMPLATES_DIR.
func (s *Server) apiTemplateCatalog(w http.ResponseWriter, r *http.Request) {
	tc, err := s.App.TemplateCatalog(r.Context(), actor(r))
	if err == nil && tc == nil {
		err = app.ErrNotFound
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, apiTemplateCatalog{ConnectionID: tc.ConnectionID.String(), Repo: tc.Repo, Branch: tc.Branch, Dir: tc.Dir, Mode: tc.Mode,
		Commit: tc.Commit, SyncedAt: tc.SyncedAt, SyncError: tc.SyncError})
}

// apiSetTemplateCatalog points the catalog at a repository branch and loads it: 200 with the settings. Saved but
// loading the templates failed (an invalid template): 200 too, with sync_error, as the settings are saved.
func (s *Server) apiSetTemplateCatalog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ConnectionID string `json:"connection_id"`
		Repo         string `json:"repo"`
		Branch       string `json:"branch"`
		Dir          string `json:"dir"`
		Mode         string `json:"mode"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	cid, err := uuid.Parse(in.ConnectionID)
	if err != nil {
		s.v1Error(w, r, &app.ValidationError{Fields: map[string]string{"connection_id": "not a connection id"}})
		return
	}
	err = s.App.SetTemplateCatalog(r.Context(), actor(r), app.CatalogSettings{ConnectionID: cid, Repo: in.Repo, Branch: in.Branch, Dir: in.Dir,
		Mode: orDefault(in.Mode, "off")})
	if ve, ok := errors.AsType[*app.ValidationError](err); ok && len(ve.Fields) == 0 { // saved; the sync error is in the settings
		err = nil
	}
	if err != nil {
		s.v1Error(w, r, bodyFields(err, map[string]string{"connection": "connection_id"}))
		return
	}
	s.apiTemplateCatalog(w, r)
}

// apiDisconnectCatalog goes back to TEMPLATES_DIR: 204.
func (s *Server) apiDisconnectCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DisconnectCatalog(r.Context(), actor(r)); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func orDefault(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}
