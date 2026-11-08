package httpserver

import (
	"net/http"
	"slices"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// Settings → Templates: the template catalog's Git repository (see app.App.PullCatalog). Without one
// the templates come from TEMPLATES_DIR.

type catalogPage struct {
	Page
	Catalog *storage.TemplateCatalog // nil: TEMPLATES_DIR
	Conns   []storage.VCSConnection
	Repos   []string // of the selected connection, for the dropdown
	ListErr bool     // listing them failed: type the repository instead
	Form    map[string]string
	Count   int
	F       *formState // a refused save
	Saved   bool
}

func (s *Server) catalogPage(w http.ResponseWriter, r *http.Request) {
	tc, err := s.App.TemplateCatalog(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	form := map[string]string{"mode": "pr"}
	if tc != nil {
		form = map[string]string{"connection": tc.ConnectionID.String(), "repo": tc.Repo, "branch": tc.Branch, "dir": tc.Dir, "mode": tc.Mode}
	}
	// another connection: the saved repo, branch and folder belong to the old one
	if c := r.URL.Query().Get("connection"); c != "" && c != form["connection"] {
		form = map[string]string{"connection": c, "mode": form["mode"]}
	}
	s.renderCatalog(w, r, http.StatusOK, form, nil)
}

func (s *Server) renderCatalog(w http.ResponseWriter, r *http.Request, status int, form map[string]string, f *formState) {
	p := catalogPage{Form: form, F: f, Count: len(s.App.Templates.List()), Saved: r.URL.Query().Has("saved")}
	var err error
	if p.Catalog, err = s.App.TemplateCatalog(r.Context(), actor(r)); err == nil {
		p.Conns, err = s.App.VCSConnections(r.Context(), actor(r))
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	for _, sc := range p.Conns {
		if sc.ID.String() != form["connection"] {
			continue
		}
		if p.Repos, err = s.App.VCSRepos(r.Context(), actor(r), sc.ID); err != nil { // typing the repo still works
			s.App.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
			p.ListErr = true
		}
		// the configured repo stays selectable even if the token no longer lists it (or GitHub's 1000 cut it off)
		if repo := form["repo"]; repo != "" && !slices.Contains(p.Repos, repo) {
			p.Repos = append([]string{repo}, p.Repos...)
		}
	}
	p.Page = Page{Section: "catalog"}
	s.render(w, r, status, "catalog.html", &p)
}

func (s *Server) saveCatalog(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	form := trimmed(f, "connection", "repo", "branch", "dir", "mode")
	connID, _ := parseID(form["connection"]) // not an id: no connection, as for an unknown one
	err := s.App.SetTemplateCatalog(r.Context(), actor(r), app.CatalogSettings{ConnectionID: connID, Repo: form["repo"], Branch: form["branch"],
		Dir: form["dir"], Mode: form["mode"]})
	if s.failed(w, r, err, "catalog", f, func(status int, fs *formState) { s.renderCatalog(w, r, status, form, fs) }) {
		return
	}
	http.Redirect(w, r, "/catalog?saved", http.StatusSeeOther)
}

func (s *Server) syncCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.App.SyncCatalog(r.Context(), actor(r)); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/catalog", http.StatusSeeOther)
}

func (s *Server) deleteCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DisconnectCatalog(r.Context(), actor(r)); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/catalog", http.StatusSeeOther)
}
