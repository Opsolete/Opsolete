package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/opsolete/opsolete/internal/auth"
)

// Account settings: the signed-in user's profile and `terraform login`
// tokens. Login is OIDC, so there are no passwords or second factors here; sessions are signed
// cookies, so there is no session list either.
func (s *Server) accountRoutes(mux *http.ServeMux) {
	mux.Handle("GET /account", http.RedirectHandler("/account/profile", http.StatusFound))
	mux.HandleFunc("GET /account/{page}", s.accountPage)
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.revokeToken)
	mux.HandleFunc("POST /account/tokens/revoke", s.revokeTokens)
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	data := map[string]any{"Page": r.PathValue("page")}
	switch r.PathValue("page") {
	case "profile": // the groups recorded at the last login decide team access
		raw, err := s.Store.UserGroups(r.Context(), u.ID)
		var groups []string
		if err == nil {
			err = json.Unmarshal(raw, &groups)
		}
		if err != nil {
			s.internalError(w, "user groups", err)
			return
		}
		data["Groups"] = groups
	case "tokens":
		tokens, err := s.Store.UserTokens(r.Context(), u.ID)
		if err != nil {
			s.internalError(w, "user tokens", err)
			return
		}
		data["Tokens"], data["Host"] = tokens, s.Registry.PublicURL.Host
	default:
		http.NotFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "account.html", data)
}

// revokeToken revokes one of the user's own tokens; at once, since every request checks it.
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if err := s.Store.RevokeUserToken(r.Context(), u.ID, r.PathValue("id")); err != nil {
		s.internalError(w, "revoke token", err)
		return
	}
	s.Log.Info("token revoked", "id", r.PathValue("id"), "user", u.Email)
	http.Redirect(w, r, "/account/tokens", http.StatusSeeOther)
}

func (s *Server) revokeTokens(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if err := s.Store.RevokeUserTokens(r.Context(), u.ID); err != nil {
		s.internalError(w, "revoke tokens", err)
		return
	}
	s.Log.Info("all tokens revoked", "user", u.Email)
	http.Redirect(w, r, "/account/tokens", http.StatusSeeOther)
}
