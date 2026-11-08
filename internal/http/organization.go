package httpserver

import "net/http"

func (s *Server) organizationPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.App.Organization(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := map[string]any{"Name": s.Organization, "Saved": r.URL.Query().Has("saved"), "DevUploads": s.Features.Editor}
	if s.Features.Deployments {
		data["Currency"] = o.Currency
	}
	if s.Features.Registry {
		data["Uploads"] = o.ModuleUploads
	}
	s.render(w, r, http.StatusOK, "organization.html", data)
}

// saveOrganization saves the settings of the components that are on.
func (s *Server) saveOrganization(w http.ResponseWriter, r *http.Request) {
	if s.Features.Deployments {
		if err := s.App.SetCurrency(r.Context(), actor(r), r.PostFormValue("currency")); err != nil {
			s.uiError(w, r, err)
			return
		}
	}
	if s.Features.Registry {
		if err := s.App.SetModuleUploads(r.Context(), actor(r), r.PostFormValue("module_uploads") == "1"); err != nil {
			s.uiError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/organization?saved", http.StatusSeeOther)
}
