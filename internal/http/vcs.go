package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// VCS providers: an admin connects GitHub or Azure DevOps with an access token. A connection is a
// credential that workspaces (within its workspace scope), registry modules (see vcs.Client.Poll) and the
// template catalog (see vcs.Client.SyncCatalog) use.
func (s *Server) vcsRoutes(mux routes) {
	mux.HandleFunc("GET /vcs", s.admin(s.vcsPage))
	mux.HandleFunc("GET /vcs/new", s.admin(s.newConnectionPage))
	mux.HandleFunc("POST /vcs", s.admin(s.addConnection))
	mux.HandleFunc("POST /vcs/{id}/delete", s.admin(s.deleteConnection))
	mux.HandleFunc("GET /vcs/{id}/settings", s.admin(s.connectionSettings))
	mux.HandleFunc("POST /vcs/{id}/settings", s.admin(s.saveConnectionSettings))
	mux.HandleFunc("POST /vcs/{id}/token", s.admin(s.replaceToken))
	if !s.Features.Registry {
		return
	}
	mux.HandleFunc("POST /registry/publish", s.admin(s.publishModule))
	mux.HandleFunc("POST /registry/{ns}/{name}/{system}/sync", s.admin(s.syncModule))
	mux.HandleFunc("POST /registry/{ns}/{name}/{system}/unlink", s.admin(s.unlinkModule))
}

// vcsPage lists the VCS connections (vcs.html).
type vcsPage struct {
	Page
	Connections []storage.VCSConnection
	Error       string
}

func (s *Server) vcsPage(w http.ResponseWriter, r *http.Request) {
	s.renderVCS(w, r, http.StatusOK, "")
}

func (s *Server) renderVCS(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	conns, err := s.App.VCSConnections(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, status, "vcs.html", &vcsPage{Page: Page{Section: "vcs"}, Connections: conns, Error: errMsg})
}

// newConnection is the add-VCS-provider wizard: 1 choose a provider,
// 2 set it up (name, URL, token), 3 advanced settings (which workspaces may link it) of the created connection.
// Return is the page that sent the user here; finishing continues it with the new connection.
type newConnection struct {
	Page
	Step      int
	Edit      bool   // step 3 alone, as the connection's settings page
	Token     bool   // on the settings page: the token was just replaced
	Return    string // "workspace", "module", "catalog" or ""
	Providers any    // vcs.Providers
	Conn      storage.VCSConnection
	F         *formState // a refused submit
	Form      map[string]string
	Picker    picker       // the selected workspaces, in step 3
	Projects  []pickerItem // the projects, selected ones checked, in step 3
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
	if p.Return != "workspace" && p.Return != "module" && p.Return != "catalog" {
		p.Return = ""
	}
	p.Providers = vcs.Providers
	if p.Step == 3 {
		// shortcut: offers the 200 most recently active workspaces, as variable sets do; add search if there are more.
		all, err := s.App.ListWorkspaces(r.Context(), actor(r), "") // an admin's: all of them
		if err != nil {
			s.internalError(w, "list workspaces", err)
			return
		}
		selected, picked := p.Conn.Workspaces, p.Conn.Projects
		if f := formOf(p.F, "scope"); f != nil { // as posted
			selected, picked = f.Input["workspace"], f.Input["project"]
		}
		p.Picker = workspacePicker("workspace", "Workspaces that may link this connection's repositories", selected, all)
		projects, err := s.App.Projects(r.Context(), actor(r))
		if err != nil {
			s.internalError(w, "list projects", err)
			return
		}
		for _, pr := range projects {
			p.Projects = append(p.Projects, pickerItem{pr.Name, projectLabel(pr), slices.Contains(picked, pr.Name)})
		}
	}
	p.Page = Page{Section: "vcs"}
	s.render(w, r, status, "vcsnew.html", &p)
}

// finishConnection continues the wizard that started the add-VCS-provider wizard, with the new connection.
func finishConnection(ret string, id uuid.UUID) string {
	connID := id.String()
	switch ret {
	case "workspace":
		return "/workspaces/new?type=vcs&conn=" + url.QueryEscape(connID)
	case "module":
		return "/registry/publish?connection=" + url.QueryEscape(connID)
	case "catalog":
		return "/catalog?connection=" + url.QueryEscape(connID)
	}
	return "/vcs"
}

// addConnection adds a connection (app.CreateVCSConnection) and continues with its advanced settings.
func (s *Server) addConnection(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	form := trimmed(f, "name", "provider", "base_url")
	id, err := s.App.CreateVCSConnection(r.Context(), actor(r), app.NewVCSConnection{Name: form["name"], Provider: form["provider"],
		BaseURL: form["base_url"], Token: f.Get("token")})
	if s.failed(w, r, err, "connection", f, func(status int, fs *formState) {
		s.renderNewConnection(w, r, status, newConnection{Step: 2, Return: f.Get("return"), F: fs, Form: form})
	}) {
		return
	}
	http.Redirect(w, r, "/vcs/new?connection="+id.String()+"&return="+url.QueryEscape(f.Get("return")), http.StatusSeeOther)
}

// connectionSettings shows a connection's scope (advanced settings): outside the wizard, as its settings page.
func (s *Server) connectionSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.renderNewConnection(w, r, http.StatusOK, newConnection{Step: 3, Edit: true, Token: r.URL.Query().Has("token"), Conn: sc, Form: scopeFormOf(sc)})
}

// saveConnectionSettings sets the workspaces that may link a connection's repositories (app.SetVCSScope).
// From the add-VCS-provider wizard (wizard=1) it finishes the wizard.
func (s *Server) saveConnectionSettings(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	wizard, ret := f.Get("wizard") != "", f.Get("return")
	form := trimmed(f, "workspaces", "workspace_scope")
	scope := ""
	if form["workspaces"] != "" {
		scope = map[bool]string{true: "all", false: "selected"}[form["workspace_scope"] == "all"]
	}
	var err error
	switch {
	case wizard && ret == "workspace" && scope == "":
		err = invalidScope("enable workspaces: your new workspace links a repository through this connection")
	case wizard && ret == "workspace" && scope == "selected":
		err = invalidScope("allow all workspaces: your new workspace can't be selected before it exists")
	default:
		err = s.App.SetVCSScope(r.Context(), actor(r), sc.ID, app.VCSScope{Scope: scope, Workspaces: f["workspace"], Projects: f["project"]})
	}
	if s.failed(w, r, err, "scope", f, func(status int, fs *formState) {
		s.renderNewConnection(w, r, status, newConnection{Step: 3, Edit: !wizard, Return: ret, Conn: sc, F: fs, Form: form})
	}) {
		return
	}
	next := "/vcs"
	if wizard {
		next = finishConnection(ret, sc.ID)
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// invalidScope is a scope the wizard's next step can't use.
func invalidScope(msg string) error {
	return &app.ValidationError{Fields: map[string]string{"workspace_scope": msg}}
}

// replaceToken swaps a connection's access token (app.ReplaceVCSToken). A refused token, or one
// replaced meanwhile, shows the form again with why.
func (s *Server) replaceToken(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.getConnection(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	_, err := s.App.ReplaceVCSToken(r.Context(), actor(r), sc.ID, f.Get("token"))
	if s.failed(w, r, err, "token", f, func(status int, fs *formState) {
		s.renderNewConnection(w, r, status, newConnection{Step: 3, Edit: true, Conn: sc, F: fs, Form: scopeFormOf(sc)})
	}) {
		return
	}
	http.Redirect(w, r, "/vcs/"+sc.ID.String()+"/settings?token", http.StatusSeeOther)
}

// scopeFormOf fills the advanced settings form from a connection.
func scopeFormOf(c storage.VCSConnection) map[string]string {
	f := map[string]string{"workspace_scope": c.WorkspaceScope}
	if c.WorkspaceScope != "" {
		f["workspaces"] = "1"
	}
	return f
}

// getConnection writes the error when it returns false.
func (s *Server) getConnection(w http.ResponseWriter, r *http.Request, id string) (storage.VCSConnection, bool) {
	cid, _ := parseID(id) // not an id: not found
	sc, err := s.App.VCSConnection(r.Context(), actor(r), cid)
	if err != nil {
		s.uiError(w, r, err)
		return sc, false
	}
	return sc, true
}

// deleteConnection deletes a connection; app.DeleteVCSConnection says what happens to what uses it.
func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: deletes nothing, as for an unknown one
	err := s.App.DeleteVCSConnection(r.Context(), actor(r), id)
	if app.IsConflict(err) {
		s.renderVCS(w, r, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/vcs", http.StatusSeeOther)
}

// publishPage is the add-module wizard (publish.html).
type publishPage struct {
	Page
	Type      string // "" choose a workflow, "vcs" or "upload"
	VCS       bool   // workflows on offer
	Upload    bool
	Step      int // vcs: 1 connect to VCS, 2 choose a repository, 3 confirm
	Conns     []storage.VCSConnection
	Providers any // vcs.Providers, for the step 1 grouping
	Conn      storage.VCSConnection
	Repos     []string
	Host      string
	Error     string     // listing the repositories failed
	F         *formState // a refused submit
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
		s.renderPublish(w, r, http.StatusOK, q.Get("connection"), nil, map[string]string{"repo": strings.TrimSpace(q.Get("repo"))})
	case t == "upload" && s.canUpload(r):
		s.renderUpload(w, r, http.StatusOK, nil, map[string]string{})
	case t == "" && (s.Features.VCS || s.canUpload(r)):
		s.render(w, r, http.StatusOK, "publish.html", &publishPage{Page: Page{Section: "registry"}, VCS: s.Features.VCS, Upload: s.canUpload(r), Host: s.App.Registry.PublicURL.Host})
	default:
		http.NotFound(w, r)
	}
}

// renderPublish shows the publish wizard: the step follows from what is chosen so far
// (no connection: pick one; no repo: pick one; else confirm the module address).
func (s *Server) renderPublish(w http.ResponseWriter, r *http.Request, status int, connID string, f *formState, form map[string]string) {
	p := &publishPage{Page: Page{Section: "registry"}, Type: "vcs", Step: 1, Providers: vcs.Providers, Host: s.App.Registry.PublicURL.Host, F: f, Form: form}
	if connID == "" {
		conns, err := s.App.VCSConnections(r.Context(), actor(r))
		if err != nil {
			s.uiError(w, r, err)
			return
		}
		p.Conns = conns
		s.render(w, r, status, "publish.html", p)
		return
	}
	sc, ok := s.getConnection(w, r, connID)
	if !ok {
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
	var err error
	if p.Repos, err = s.App.VCSRepos(r.Context(), actor(r), sc.ID); err != nil {
		p.Error = err.Error()
	}
	s.render(w, r, status, "publish.html", p)
}

// fillFromRepo fills in what is left empty with the address the repository suggests (app.RepoModuleAddress).
func fillFromRepo(form map[string]string) {
	ns, name, sys := app.RepoModuleAddress(form["repo"])
	for k, v := range map[string]string{"namespace": ns, "name": name, "system": sys} {
		if form[k] == "" {
			form[k] = v
		}
	}
}

// publishModule links a repo to a registry module and publishes its tags right away (app.PublishVCSModule).
func (s *Server) publishModule(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	form := trimmed(f, "repo", "dir", "namespace", "name", "system")
	connID := f.Get("connection")
	cid, _ := parseID(connID) // not an id: not found
	m, added, err := s.App.PublishVCSModule(r.Context(), actor(r), app.NewVCSModule{ConnectionID: cid, Repo: form["repo"], Dir: form["dir"],
		Namespace: form["namespace"], Name: form["name"], System: form["system"]})
	if s.failed(w, r, err, "module", f, func(status int, fs *formState) { s.renderPublish(w, r, status, connID, fs, form) }) {
		return
	}
	redirectToVersion(w, r, m, added)
}

func (s *Server) syncModule(w http.ResponseWriter, r *http.Request) {
	m, added, err := s.App.SyncVCSModule(r.Context(), actor(r), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	redirectToVersion(w, r, m, added)
}

// unlinkModule stops publishing a module from its repository (app.UnlinkVCSModule).
func (s *Server) unlinkModule(w http.ResponseWriter, r *http.Request) {
	if err := s.App.UnlinkVCSModule(r.Context(), actor(r), r.PathValue("ns"), r.PathValue("name"), r.PathValue("system")); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/registry", http.StatusSeeOther)
}

// redirectToVersion goes to the newest version a sync added, else to the registry, which shows sync errors.
func redirectToVersion(w http.ResponseWriter, r *http.Request, m storage.VCSModule, added []string) {
	if len(added) == 0 {
		http.Redirect(w, r, "/registry", http.StatusSeeOther)
		return
	}
	registry.SortVersions(added)
	http.Redirect(w, r, fmt.Sprintf("/registry/%s/%s/%s/%s", m.Namespace, m.Name, m.System, added[0]), http.StatusSeeOther)
}
