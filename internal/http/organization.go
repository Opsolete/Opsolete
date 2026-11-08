package httpserver

import "net/http"

func (s *Server) organizationPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Name": s.Organization, "Saved": r.URL.Query().Has("saved"), "DevUploads": s.Features.Editor}
	var err error
	if s.Features.Deployments {
		if data["Currency"], err = s.App.Store.Currency(r.Context()); err != nil {
			s.internalError(w, "currency", err)
			return
		}
	}
	if s.Features.Registry {
		if data["Uploads"], err = s.App.Store.ModuleUploads(r.Context()); err != nil {
			s.internalError(w, "module uploads", err)
			return
		}
	}
	s.render(w, r, http.StatusOK, "organization.html", data)
}

func (s *Server) saveOrganization(w http.ResponseWriter, r *http.Request) {
	if s.Features.Deployments {
		c := r.PostFormValue("currency")
		if c != "USD" && c != "EUR" {
			http.Error(w, "currency must be USD or EUR", http.StatusBadRequest)
			return
		}
		if err := s.App.Store.SetCurrency(r.Context(), c); err != nil {
			s.internalError(w, "set currency", err)
			return
		}
	}
	if s.Features.Registry {
		if err := s.App.Store.SetModuleUploads(r.Context(), r.PostFormValue("module_uploads") == "1"); err != nil {
			s.internalError(w, "set module uploads", err)
			return
		}
	}
	http.Redirect(w, r, "/organization?saved", http.StatusSeeOther)
}
