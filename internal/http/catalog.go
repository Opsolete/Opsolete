package httpserver

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Settings → Templates: the template catalog's Git repository (see vcs.SyncCatalog). Without one
// the templates come from TEMPLATES_DIR.

var dirRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,255}$`)

type catalogPage struct {
	Catalog *storage.TemplateCatalog // nil: TEMPLATES_DIR
	Conns   []storage.VCSConnection
	Repos   []string // of the selected connection, for the dropdown
	ListErr bool     // listing them failed: type the repository instead
	Form    map[string]string
	Count   int
	Error   string
	Saved   bool
}

func (s *Server) catalogPage(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{"mode": "pr"}
	tc, err := s.Store.GetTemplateCatalog(r.Context())
	if err == nil {
		form = map[string]string{"connection": tc.ConnectionID, "repo": tc.Repo, "branch": tc.Branch, "dir": tc.Dir, "mode": tc.Mode}
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.internalError(w, "template catalog", err)
		return
	}
	if c := r.URL.Query().Get("connection"); c != "" {
		form["connection"] = c
	}
	s.renderCatalog(w, r, http.StatusOK, form, "")
}

func (s *Server) renderCatalog(w http.ResponseWriter, r *http.Request, status int, form map[string]string, errMsg string) {
	p := catalogPage{Form: form, Error: errMsg, Count: len(s.Templates.List()), Saved: r.URL.Query().Has("saved")}
	tc, err := s.Store.GetTemplateCatalog(r.Context())
	if err == nil {
		p.Catalog = &tc
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.internalError(w, "template catalog", err)
		return
	}
	if p.Conns, err = s.Store.VCSConnections(r.Context()); err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	for _, sc := range p.Conns {
		if sc.ID != form["connection"] {
			continue
		}
		c, err := vcs.ConnFor(s.SecretKey, sc)
		if err == nil {
			p.Repos, err = c.Repos(r.Context())
		}
		if err != nil { // typing the repo still works
			s.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
			p.ListErr = true
		}
		// the configured repo stays selectable even if the token no longer lists it (or GitHub's 1000 cut it off)
		if repo := form["repo"]; repo != "" && !slices.Contains(p.Repos, repo) {
			p.Repos = append([]string{repo}, p.Repos...)
		}
	}
	s.render(w, r, status, "catalog.html", p)
}

func (s *Server) saveCatalog(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"connection", "repo", "branch", "dir", "mode"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	tc := storage.TemplateCatalog{ConnectionID: form["connection"], Repo: form["repo"], Branch: form["branch"],
		Dir: strings.Trim(form["dir"], "/"), Mode: form["mode"]}
	if tc.Branch == "" {
		tc.Branch = "main"
	}
	if tc.Dir != "" {
		tc.Dir = path.Clean(tc.Dir)
	}
	fail := func(msg string) { s.renderCatalog(w, r, http.StatusUnprocessableEntity, form, msg) }
	switch {
	case tc.Mode != "off" && tc.Mode != "commit" && tc.Mode != "pr":
		fail("choose what saving a template does")
		return
	case !branchRe.MatchString(tc.Branch) || strings.Contains(tc.Branch, ".."):
		fail(fmt.Sprintf("%q is not a branch name", tc.Branch))
		return
	case !dirRe.MatchString(tc.Dir) || tc.Dir == ".." || strings.HasPrefix(tc.Dir, "../"):
		fail(fmt.Sprintf("%q is not a folder of the repository", form["dir"]))
		return
	}
	sc, err := s.Store.GetVCSConnection(r.Context(), tc.ConnectionID)
	if errors.Is(err, sql.ErrNoRows) {
		fail("choose a VCS connection")
		return
	} else if err != nil {
		s.internalError(w, "vcs connection", err)
		return
	}
	c, err := vcs.ConnFor(s.SecretKey, sc)
	if err == nil {
		_, err = c.Head(r.Context(), tc.Repo, tc.Branch)
	}
	if err != nil {
		fail(fmt.Sprintf("can't read branch %s of %q: %v", tc.Branch, tc.Repo, err))
		return
	}
	if err := s.Store.SetTemplateCatalog(r.Context(), tc); err != nil {
		s.internalError(w, "set template catalog", err)
		return
	}
	if err := vcs.SyncCatalog(r.Context(), s.Store, s.SecretKey, s.Templates); err != nil {
		s.renderCatalog(w, r, http.StatusUnprocessableEntity, form, "Saved, but loading the templates failed: "+err.Error())
		return
	}
	http.Redirect(w, r, "/catalog?saved", http.StatusSeeOther)
}

func (s *Server) syncCatalog(w http.ResponseWriter, r *http.Request) {
	if err := vcs.SyncCatalog(r.Context(), s.Store, s.SecretKey, s.Templates); err != nil {
		s.Log.Warn("template catalog sync", "err", err) // shown on the page as the sync error
	}
	http.Redirect(w, r, "/catalog", http.StatusSeeOther)
}

func (s *Server) deleteCatalog(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteTemplateCatalog(r.Context()); err != nil {
		s.internalError(w, "delete template catalog", err)
		return
	}
	if err := s.Templates.Reopen(); err != nil {
		s.internalError(w, "load TEMPLATES_DIR", err)
		return
	}
	http.Redirect(w, r, "/catalog", http.StatusSeeOther)
}
