package httpserver

import (
	"net/http"

	"github.com/opsolete/opsolete/internal/storage"
)

// Account settings: the signed-in user's profile and `terraform login`
// tokens. Login is OIDC, so there are no passwords or second factors here; sessions are signed
// cookies, so there is no session list either.
func (s *Server) accountRoutes(mux routes) {
	mux.Handle("GET /account", http.RedirectHandler("/account/profile", http.StatusFound))
	mux.HandleFunc("GET /account/{page}", s.accountPage)
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.revokeToken)
	mux.HandleFunc("POST /account/tokens/revoke", s.revokeTokens)
}

// accountPage is account.html: Page.Sub is "profile" or "tokens".
type accountPage struct {
	Page
	Groups []string // profile: recorded at the last login, they decide team access
	Tokens []storage.UserToken
	Host   string // tokens: for `terraform login`
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	p := &accountPage{Page: Page{Section: "account", Sub: r.PathValue("page")}}
	var err error
	switch p.Sub {
	case "profile":
		p.Groups, err = s.App.UserGroups(r.Context(), actor(r))
	case "tokens":
		p.Tokens, err = s.App.UserTokens(r.Context(), actor(r))
		p.Host = s.App.Registry.PublicURL.Host
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "account.html", p)
}

// revokeToken revokes one of the user's own tokens; at once, since every request checks it.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "id") // not an id: revokes nothing, as for an unknown one
	if err := s.App.RevokeUserToken(r.Context(), actor(r), id); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/tokens", http.StatusSeeOther)
}

func (s *Server) revokeTokens(w http.ResponseWriter, r *http.Request) {
	if err := s.App.RevokeUserTokens(r.Context(), actor(r)); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/tokens", http.StatusSeeOther)
}
