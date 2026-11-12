package httpserver

import (
	"net/http"

	"github.com/opsolete/opsolete/internal/storage"
)

// Runners (Organization settings): each runner has its own token and pool (app/runners.go). Admins only.

// runnersPage is runners.html; Token is a runner's new token, shown once after it's created.
type runnersPage struct {
	Page
	Runners []storage.Runner
	Created string // the new runner's name, with Token
	Token   string
	Server  string // the server's URL, for OPS_SERVER_URL
	F       *formState
}

func (s *Server) runnersPage(w http.ResponseWriter, r *http.Request) {
	s.renderRunners(w, r, http.StatusOK, runnersPage{})
}

func (s *Server) renderRunners(w http.ResponseWriter, r *http.Request, status int, p runnersPage) {
	list, err := s.App.Runners(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	p.Page, p.Runners, p.Server = Page{Section: "runners"}, list, s.App.Registry.PublicURL.String()
	s.render(w, r, status, "runners.html", &p)
}

// createRunner registers a runner and shows its token, once: no redirect, the page is the only copy.
func (s *Server) createRunner(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	token, err := s.App.CreateRunner(r.Context(), actor(r), f.Get("name"), f.Get("pool"))
	if s.failed(w, r, err, "runner", f, func(status int, fs *formState) { s.renderRunners(w, r, status, runnersPage{F: fs}) }) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderRunners(w, r, http.StatusOK, runnersPage{Created: f.Get("name"), Token: token})
}

func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: not found
	if err := s.App.DeleteRunner(r.Context(), actor(r), id); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/organization/runners", http.StatusSeeOther)
}
