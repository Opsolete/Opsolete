package httpserver

import "net/http"

func (s *Server) organizationPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.Currency(r.Context())
	if err != nil {
		s.internalError(w, "currency", err)
		return
	}
	s.render(w, r, http.StatusOK, "organization.html", map[string]any{"Name": s.Organization, "Currency": c, "Saved": r.URL.Query().Has("saved")})
}

func (s *Server) saveOrganization(w http.ResponseWriter, r *http.Request) {
	c := r.PostFormValue("currency")
	if c != "USD" && c != "EUR" {
		http.Error(w, "currency must be USD or EUR", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetCurrency(r.Context(), c); err != nil {
		s.internalError(w, "set currency", err)
		return
	}
	http.Redirect(w, r, "/organization?saved", http.StatusSeeOther)
}
