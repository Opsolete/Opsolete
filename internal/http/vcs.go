package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/registry"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/vcs"
)

// VCS providers, HCP Terraform style: an admin connects GitHub or Azure DevOps with an access
// token, then publishes a repo as a registry module. Its semver tags become versions (see vcs.Poll).
func (s *Server) vcsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /vcs", s.admin(s.vcsPage))
	mux.HandleFunc("POST /vcs", s.admin(s.addConnection))
	mux.HandleFunc("POST /vcs/{id}/delete", s.admin(s.deleteConnection))
	mux.HandleFunc("GET /registry/publish", s.admin(s.publishPage))
	mux.HandleFunc("POST /registry/publish", s.admin(s.publishModule))
	mux.HandleFunc("POST /registry/{ns}/{name}/{system}/sync", s.admin(s.syncModule))
}

type vcsPage struct {
	Connections []storage.VCSConnection
	Error       string
	Form        map[string]string
}

func (s *Server) vcsPage(w http.ResponseWriter, r *http.Request) {
	s.renderVCS(w, r, http.StatusOK, "", map[string]string{"provider": vcs.GitHub})
}

func (s *Server) renderVCS(w http.ResponseWriter, r *http.Request, status int, errMsg string, form map[string]string) {
	conns, err := s.Store.VCSConnections(r.Context())
	if err != nil {
		s.internalError(w, "list vcs connections", err)
		return
	}
	s.render(w, r, status, "vcs.html", vcsPage{Connections: conns, Error: errMsg, Form: form})
}

// addConnection checks the token against the provider (by listing repos) before storing it sealed.
func (s *Server) addConnection(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"name", "provider", "base_url"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	fail := func(msg string) { s.renderVCS(w, r, http.StatusUnprocessableEntity, msg, form) }
	token := strings.TrimSpace(r.PostFormValue("token"))
	if form["name"] == "" || len(form["name"]) > 64 || token == "" {
		fail("name (up to 64 characters) and access token are required")
		return
	}
	baseURL, err := vcs.NormalizeURL(form["provider"], form["base_url"])
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := (vcs.Conn{Provider: form["provider"], BaseURL: baseURL, Token: token}).Repos(ctx); err != nil {
		fail("connection check failed: " + err.Error())
		return
	}
	sealed, err := vcs.Seal(s.SecretKey, token)
	if err != nil {
		s.internalError(w, "seal vcs token", err)
		return
	}
	err = s.Store.CreateVCSConnection(r.Context(), storage.VCSConnection{Name: form["name"], Provider: form["provider"], BaseURL: baseURL, Token: sealed})
	if errors.Is(err, storage.ErrExists) {
		fail("a connection with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, "create vcs connection", err)
		return
	}
	s.Log.Info("vcs connection added", "name", form["name"], "provider", form["provider"], "url", baseURL, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/vcs", http.StatusSeeOther)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteVCSConnection(r.Context(), r.PathValue("id")); err != nil {
		s.internalError(w, "delete vcs connection", err)
		return
	}
	s.Log.Info("vcs connection deleted", "id", r.PathValue("id"), "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/vcs", http.StatusSeeOther)
}

type publishPage struct {
	Conn  storage.VCSConnection
	Repos []string
	Host  string
	Error string
	Form  map[string]string
}

func (s *Server) publishPage(w http.ResponseWriter, r *http.Request) {
	s.renderPublish(w, r, http.StatusOK, r.URL.Query().Get("connection"), "", map[string]string{})
}

// renderPublish shows the repo picker of one connection; listing errors are shown, typing a repo still works.
func (s *Server) renderPublish(w http.ResponseWriter, r *http.Request, status int, connID, errMsg string, form map[string]string) {
	sc, err := s.Store.GetVCSConnection(r.Context(), connID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "get vcs connection", err)
		return
	}
	p := publishPage{Conn: sc, Host: s.Registry.PublicURL.Host, Error: errMsg, Form: form}
	if c, err := vcs.ConnFor(s.SecretKey, sc); err != nil {
		p.Error = err.Error()
	} else if p.Repos, err = c.Repos(r.Context()); err != nil && p.Error == "" {
		p.Error = "could not list repositories: " + err.Error()
	}
	s.render(w, r, status, "publish.html", p)
}

// HCP's module repo naming convention; used to fill in name and system when left empty.
var tfRepoRe = regexp.MustCompile(`^terraform-([a-z0-9]+)-([a-z0-9][a-z0-9_-]*)$`)

// publishModule links a repo to a registry module and publishes its tags right away.
func (s *Server) publishModule(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"repo", "namespace", "name", "system"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	connID := r.PostFormValue("connection")
	if _, err := s.Store.GetVCSConnection(r.Context(), connID); errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.internalError(w, "get vcs connection", err)
		return
	}
	fail := func(msg string) { s.renderPublish(w, r, http.StatusUnprocessableEntity, connID, msg, form) }
	if m := tfRepoRe.FindStringSubmatch(strings.ToLower(path.Base(form["repo"]))); m != nil {
		if form["system"] == "" {
			form["system"] = m[1]
		}
		if form["name"] == "" {
			form["name"] = m[2]
		}
	}
	ns, name, sys, _, err := s.Registry.Address(form["namespace"], form["name"], form["system"])
	if err != nil {
		fail(err.Error())
		return
	}
	if !strings.Contains(form["repo"], "/") {
		fail("choose a repository")
		return
	}
	m := storage.VCSModule{Namespace: ns, Name: name, System: sys, ConnectionID: connID, Repo: form["repo"]}
	if err := s.Store.LinkVCSModule(r.Context(), m); errors.Is(err, storage.ErrExists) {
		fail(fmt.Sprintf("%s/%s/%s is already published from a repository", ns, name, sys))
		return
	} else if err != nil {
		s.internalError(w, "link vcs module", err)
		return
	}
	s.Log.Info("module linked to vcs", "module", ns+"/"+name+"/"+sys, "repo", m.Repo, "user", auth.FromContext(r.Context()).Email)
	s.syncAndRedirect(w, r, m)
}

func (s *Server) syncModule(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetVCSModule(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "get vcs module", err)
		return
	}
	s.syncAndRedirect(w, r, m)
}

// syncAndRedirect goes to the newest version it published, else to the registry, which shows sync errors.
func (s *Server) syncAndRedirect(w http.ResponseWriter, r *http.Request, m storage.VCSModule) {
	added, err := vcs.Sync(r.Context(), s.Store, s.SecretKey, m)
	if err != nil {
		s.Log.Warn("vcs sync", "module", m.Namespace+"/"+m.Name+"/"+m.System, "err", err)
	}
	if len(added) == 0 {
		http.Redirect(w, r, "/registry", http.StatusSeeOther)
		return
	}
	registry.SortVersions(added)
	http.Redirect(w, r, fmt.Sprintf("/registry/%s/%s/%s/%s", m.Namespace, m.Name, m.System, added[0]), http.StatusSeeOther)
}
