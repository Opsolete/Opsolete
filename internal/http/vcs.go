package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// VCS providers: an admin connects GitHub or Azure DevOps with an access
// token, then publishes a repo as a registry module. Its semver tags become versions (see vcs.Poll).
func (s *Server) vcsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /vcs", s.admin(s.vcsPage))
	mux.HandleFunc("GET /vcs/new", s.admin(s.newConnectionPage))
	mux.HandleFunc("POST /vcs", s.admin(s.addConnection))
	mux.HandleFunc("POST /vcs/{id}/delete", s.admin(s.deleteConnection))
	mux.HandleFunc("GET /vcs/{id}/settings", s.admin(s.connectionSettings))
	mux.HandleFunc("POST /vcs/{id}/settings", s.admin(s.saveConnectionSettings))
	mux.HandleFunc("POST /vcs/{id}/workspaces", s.admin(s.attachConnectionWorkspace))
	mux.HandleFunc("POST /registry/publish", s.admin(s.publishModule))
	mux.HandleFunc("POST /registry/{ns}/{name}/{system}/sync", s.admin(s.syncModule))
}

type vcsPage struct {
	Connections []storage.VCSConnection
	Error       string
}

func (s *Server) vcsPage(w http.ResponseWriter, r *http.Request) {
	s.renderVCS(w, r, http.StatusOK, "")
}

func (s *Server) renderVCS(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	conns, err := s.Store.VCSConnections(r.Context())
	if err != nil {
		s.internalError(w, "list vcs connections", err)
		return
	}
	s.render(w, r, status, "vcs.html", vcsPage{Connections: conns, Error: errMsg})
}

// newConnection is the add-VCS-provider wizard, like HCP Terraform's: 1 choose a provider,
// 2 set it up (name, URL, token), 3 advanced settings (which workspaces may link it) of the created connection.
// Return is the wizard that sent the user here; finishing continues it with the new connection.
type newConnection struct {
	Step       int
	Edit       bool   // step 3 alone, as the connection's settings page
	Return     string // "workspace", "module" or ""
	Next       string // where finishing goes
	Providers  any    // vcs.Providers
	Conn       storage.VCSConnection
	Error      string
	Form       map[string]string
	Attachable []string // workspaces not selected yet, in step 3
}

func (s *Server) newConnectionPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if id := q.Get("connection"); id != "" {
		sc, ok := s.getConnection(w, r, id)
		if !ok {
			return
		}
		form := scopeFormOf(sc)
		if q.Get("return") == "workspace" { // that is what the connection is for
			form["workspaces"] = "1"
		}
		s.renderNewConnection(w, r, http.StatusOK, newConnection{Step: 3, Return: q.Get("return"), Conn: sc, Form: form})
		return
	}
	step := 1
	for _, p := range vcs.Providers {
		if p.ID == q.Get("provider") {
			step = 2
		}
	}
	s.renderNewConnection(w, r, http.StatusOK, newConnection{Step: step, Return: q.Get("return"), Form: map[string]string{"provider": q.Get("provider")}})
}

func (s *Server) renderNewConnection(w http.ResponseWriter, r *http.Request, status int, p newConnection) {
	if p.Return != "workspace" && p.Return != "module" {
		p.Return = ""
	}
	p.Providers, p.Next = vcs.Providers, finishConnection(p.Return, p.Conn.ID)
	if p.Step == 3 {
		// shortcut: offers the 200 most recently active workspaces, as variable sets do; add search if there are more.
		all, err := s.Store.ListWorkspaces(r.Context(), storage.Viewer{Admin: true}) // admin-only page
		if err != nil {
			s.internalError(w, "list workspaces", err)
			return
		}
		for _, ws := range all {
			if ws.TemplateID == "" && !slices.Contains(p.Conn.Workspaces, ws.Name) { // template workspaces never link a repository
				p.Attachable = append(p.Attachable, ws.Name)
			}
		}
	}
	s.render(w, r, status, "vcsnew.html", p)
}

// finishConnection continues the wizard that started the add-VCS-provider wizard, with the new connection.
func finishConnection(ret, connID string) string {
	switch ret {
	case "workspace":
		return "/workspaces/new?type=vcs&conn=" + url.QueryEscape(connID)
	case "module":
		return "/registry/publish?connection=" + url.QueryEscape(connID)
	}
	return "/vcs"
}

// addConnection checks the token against the provider (by listing repos) before storing it sealed.
func (s *Server) addConnection(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"name", "provider", "base_url"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	fail := func(msg string) {
		s.renderNewConnection(w, r, http.StatusUnprocessableEntity, newConnection{Step: 2, Return: r.PostFormValue("return"), Error: msg, Form: form})
	}
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
	id, err := s.Store.CreateVCSConnection(r.Context(), storage.VCSConnection{Name: form["name"], Provider: form["provider"], BaseURL: baseURL, Token: sealed})
	if errors.Is(err, storage.ErrExists) {
		fail("a connection with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, "create vcs connection", err)
		return
	}
	s.Log.Info("vcs connection added", "name", form["name"], "provider", form["provider"], "url", baseURL, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/vcs/new?connection="+id+"&return="+url.QueryEscape(r.PostFormValue("return")), http.StatusSeeOther)
}

// connectionSettings shows a connection's scope (advanced settings): outside the wizard, as its settings page.
func (s *Server) connectionSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.renderNewConnection(w, r, http.StatusOK, newConnection{Step: 3, Edit: true, Conn: sc, Form: scopeFormOf(sc)})
}

// attachConnectionWorkspace selects a workspace (by name) for the connection, which scopes it to
// selected workspaces, or with detach=<name> deselects one: runs of a deselected workspace linked
// through it fail when they start. The scope form's Attach and Detach buttons post here.
func (s *Server) attachConnectionWorkspace(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	name, attach := r.PostFormValue("detach"), false
	if name == "" {
		name, attach = r.PostFormValue("workspace"), true
	}
	err := s.Store.AttachVCSWorkspace(r.Context(), sc.ID, name, attach)
	if err == nil && attach && sc.WorkspaceScope != "selected" {
		err = s.Store.SetVCSScope(r.Context(), sc.ID, "selected", sc.Modules)
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "choose a workspace", http.StatusBadRequest)
		return
	} else if err != nil {
		s.internalError(w, "attach vcs workspace", err)
		return
	}
	s.Log.Info("vcs connection workspace", "id", sc.ID, "workspace", name, "attach", attach, "user", auth.FromContext(r.Context()).Email)
	next := "/vcs/" + sc.ID + "/settings"
	if r.PostFormValue("wizard") != "" {
		next = "/vcs/new?connection=" + sc.ID + "&return=" + url.QueryEscape(r.PostFormValue("return"))
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// saveConnectionSettings sets what a connection may be used for; runs of linked workspaces no
// longer allowed fail when they start, and linked modules keep syncing.
// From the add-VCS-provider wizard (wizard=1) it finishes the wizard.
func (s *Server) saveConnectionSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	wizard, ret := r.PostFormValue("wizard") != "", r.PostFormValue("return")
	form := map[string]string{}
	for _, k := range []string{"workspaces", "workspace_scope", "modules"} {
		form[k] = r.PostFormValue(k)
	}
	scope := ""
	if form["workspaces"] != "" {
		scope = map[bool]string{true: "all", false: "selected"}[form["workspace_scope"] == "all"]
	}
	var err error
	switch {
	case wizard && ret == "workspace" && scope == "":
		err = errors.New("enable workspaces: your new workspace links a repository through this connection")
	case wizard && ret == "workspace" && scope == "selected":
		err = errors.New("allow all workspaces: your new workspace can't be selected before it exists")
	case wizard && ret == "module" && form["modules"] == "":
		err = errors.New("enable modules: you are publishing a module from this connection")
	}
	if err != nil {
		s.renderNewConnection(w, r, http.StatusUnprocessableEntity, newConnection{Step: 3, Edit: !wizard, Return: ret, Conn: sc, Error: err.Error(), Form: form})
		return
	}
	if err := s.Store.SetVCSScope(r.Context(), sc.ID, scope, form["modules"] != ""); err != nil {
		s.internalError(w, "set vcs scope", err)
		return
	}
	s.Log.Info("vcs connection scope", "id", sc.ID, "workspace_scope", scope, "modules", form["modules"] != "", "user", auth.FromContext(r.Context()).Email)
	next := "/vcs"
	if wizard {
		next = finishConnection(ret, sc.ID)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// scopeFormOf fills the advanced settings form from a connection.
func scopeFormOf(c storage.VCSConnection) map[string]string {
	f := map[string]string{"workspace_scope": c.WorkspaceScope}
	if c.WorkspaceScope != "" {
		f["workspaces"] = "1"
	}
	if c.Modules {
		f["modules"] = "1"
	}
	return f
}

// getConnection writes a 404 or 500 when it returns false.
func (s *Server) getConnection(w http.ResponseWriter, r *http.Request, id string) (storage.VCSConnection, bool) {
	sc, err := s.Store.GetVCSConnection(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return sc, false
	}
	if err != nil {
		s.internalError(w, "get vcs connection", err)
		return sc, false
	}
	return sc, true
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
	Type      string // "" choose a workflow, "vcs" or "upload"
	VCS       bool   // workflows on offer
	Upload    bool
	Step      int // vcs: 1 connect to VCS, 2 choose a repository, 3 confirm
	Conns     []storage.VCSConnection
	Providers any // vcs.Providers, for the step 1 grouping
	Conn      storage.VCSConnection
	Repos     []string
	Host      string
	Error     string
	Form      map[string]string
}

// publishPage is the add-module wizard: choose a workflow (?type=), then its steps.
func (s *Server) publishPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	t := q.Get("type")
	if q.Get("connection") != "" {
		t = "vcs"
	}
	switch {
	case t == "vcs" && s.Features.VCS:
		s.renderPublish(w, r, http.StatusOK, q.Get("connection"), "", map[string]string{"repo": strings.TrimSpace(q.Get("repo"))})
	case t == "upload" && s.canEdit(r):
		s.renderUpload(w, r, http.StatusOK, "", map[string]string{})
	case t == "":
		s.render(w, r, http.StatusOK, "publish.html", publishPage{VCS: s.Features.VCS, Upload: s.canEdit(r), Host: s.Registry.PublicURL.Host})
	default:
		http.NotFound(w, r)
	}
}

// renderPublish shows the publish wizard: the step follows from what is chosen so far
// (no connection: pick one; no repo: pick one; else confirm the module address).
func (s *Server) renderPublish(w http.ResponseWriter, r *http.Request, status int, connID, errMsg string, form map[string]string) {
	p := publishPage{Type: "vcs", Step: 1, Providers: vcs.Providers, Host: s.Registry.PublicURL.Host, Error: errMsg, Form: form}
	if connID == "" {
		conns, err := s.Store.VCSConnections(r.Context())
		if err != nil {
			s.internalError(w, "vcs connections", err)
			return
		}
		for _, c := range conns {
			if c.Modules {
				p.Conns = append(p.Conns, c)
			}
		}
		s.render(w, r, status, "publish.html", p)
		return
	}
	sc, ok := s.getConnection(w, r, connID)
	if !ok {
		return
	}
	if !sc.Modules {
		http.Error(w, "this VCS connection doesn't allow publishing modules", http.StatusForbidden)
		return
	}
	p.Conn = sc
	if form["repo"] != "" {
		p.Step = 3
		fillFromRepo(form)
		s.render(w, r, status, "publish.html", p)
		return
	}
	// listing errors are shown; typing a repo still works
	p.Step = 2
	if c, err := vcs.ConnFor(s.SecretKey, sc); err != nil {
		p.Error = err.Error()
	} else if p.Repos, err = c.Repos(r.Context()); err != nil && p.Error == "" {
		p.Error = "could not list repositories: " + err.Error()
	}
	s.render(w, r, status, "publish.html", p)
}

// Terraform's module repo naming convention (terraform-<system>-<name>); used to fill in name and system when left empty.
var tfRepoRe = regexp.MustCompile(`^terraform-([a-z0-9]+)-([a-z0-9][a-z0-9_-]*)$`)

func fillFromRepo(form map[string]string) {
	if m := tfRepoRe.FindStringSubmatch(strings.ToLower(path.Base(form["repo"]))); m != nil {
		if form["system"] == "" {
			form["system"] = m[1]
		}
		if form["name"] == "" {
			form["name"] = m[2]
		}
	}
}

// publishModule links a repo to a registry module and publishes its tags right away.
func (s *Server) publishModule(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"repo", "namespace", "name", "system"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	connID := r.PostFormValue("connection")
	if sc, ok := s.getConnection(w, r, connID); !ok {
		return
	} else if !sc.Modules {
		http.Error(w, "this VCS connection doesn't allow publishing modules", http.StatusForbidden)
		return
	}
	fail := func(msg string) { s.renderPublish(w, r, http.StatusUnprocessableEntity, connID, msg, form) }
	fillFromRepo(form)
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
