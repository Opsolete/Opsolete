package httpserver

import (
	"net/http"

	"github.com/opsolete/opsolete/internal/storage"
)

// API tokens (Organization settings): service tokens for automation, such as the Terraform provider: admin rights,
// not a person's, audited as token:<name> (app/servicetokens.go). Admins only.

// apiTokensPage is api_tokens.html; Token is a new token, shown once after it's created.
type apiTokensPage struct {
	Page
	Tokens  []storage.ServiceToken
	Created string // the new token's name, with Token
	Token   string
	F       *formState
}

func (s *Server) apiTokensPage(w http.ResponseWriter, r *http.Request) {
	s.renderAPITokens(w, r, http.StatusOK, apiTokensPage{})
}

func (s *Server) renderAPITokens(w http.ResponseWriter, r *http.Request, status int, p apiTokensPage) {
	list, err := s.App.ServiceTokens(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	p.Page, p.Tokens = Page{Section: "api-tokens"}, list
	s.render(w, r, status, "api_tokens.html", &p)
}

// createAPIToken creates a service token and shows it, once: no redirect, the page is the only copy.
func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	token, err := s.App.CreateServiceToken(r.Context(), actor(r), f.Get("name"), nil)
	if s.failed(w, r, err, "token", f, func(status int, fs *formState) { s.renderAPITokens(w, r, status, apiTokensPage{F: fs}) }) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderAPITokens(w, r, http.StatusOK, apiTokensPage{Created: f.Get("name"), Token: token})
}

func (s *Server) revokeAPIToken(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: not found
	if err := s.App.RevokeServiceToken(r.Context(), actor(r), id); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/organization/api-tokens", http.StatusSeeOther)
}
