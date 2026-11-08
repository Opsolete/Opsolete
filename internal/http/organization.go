package httpserver

import "net/http"

func (s *Server) organizationPage(w http.ResponseWriter, r *http.Request) {
	s.renderOrganization(w, r, http.StatusOK, nil)
}

// renderOrganization shows the organization's settings; f: a refused save.
func (s *Server) renderOrganization(w http.ResponseWriter, r *http.Request, status int, f *formState) {
	o, err := s.App.Organization(r.Context(), actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	data := map[string]any{"Name": s.Organization, "Saved": r.URL.Query().Has("saved"), "DevUploads": s.Features.Editor, "F": f}
	if s.Features.Deployments {
		data["Currency"] = o.Currency
	}
	if s.Features.Registry {
		data["Uploads"] = o.ModuleUploads
	}
	s.render(w, r, status, "organization.html", data)
}

// saveOrganization saves the settings of the components that are on.
func (s *Server) saveOrganization(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	var err error
	if s.Features.Deployments {
		err = s.App.SetCurrency(r.Context(), actor(r), f.Get("currency"))
	}
	if err == nil && s.Features.Registry {
		err = s.App.SetModuleUploads(r.Context(), actor(r), f.Get("module_uploads") == "1")
	}
	if s.failed(w, r, err, "organization", f, func(status int, fs *formState) { s.renderOrganization(w, r, status, fs) }) {
		return
	}
	http.Redirect(w, r, "/organization?saved", http.StatusSeeOther)
}
