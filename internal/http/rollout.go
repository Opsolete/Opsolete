package httpserver

import (
	"cmp"
	"net/http"
	"net/url"
	"strconv"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Template rollouts (app.StartRollout): the page of a template's uses and its rollout, and its actions.

type rolloutPage struct {
	T        templates.Template
	Rows     []app.RolloutRow
	Pending  int
	Projects []storage.Project
	Project  string           // the filter, a name; "" all projects
	Rollout  *storage.Rollout // the template's latest rollout
	OpenRuns int              // its open runs
}

func (s *Server) rolloutPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project := r.URL.Query().Get("project")
	v, err := s.App.Rollouts(ctx, actor(r), r.PathValue("id"), project)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	projects, err := s.App.Projects(ctx, actor(r))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "rollout.html", rolloutPage{T: v.Template, Rows: v.Rows, Pending: v.Pending, Projects: projects,
		Project: project, Rollout: v.Rollout, OpenRuns: v.OpenRuns})
}

// startRollout starts a rollout to the version the page showed, in one project or all.
func (s *Server) startRollout(w http.ResponseWriter, r *http.Request) {
	batch, err := strconv.Atoi(cmp.Or(r.PostFormValue("batch"), "0"))
	if err != nil {
		batch = -1 // app refuses it
	}
	in := app.NewRollout{Source: r.PostFormValue("source"), Ref: r.PostFormValue("ref"), Project: r.PostFormValue("project"), BatchSize: batch}
	if _, err := s.App.StartRollout(r.Context(), actor(r), r.PathValue("id"), in); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/templates/"+r.PathValue("id")+"/rollout?project="+url.QueryEscape(in.Project), http.StatusSeeOther)
}

// rolloutAction pauses, resumes or stops the template's open rollout.
func (s *Server) rolloutAction(w http.ResponseWriter, r *http.Request) {
	ro, err := s.App.RolloutAction(r.Context(), actor(r), r.PathValue("id"), r.PathValue("action"))
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/templates/"+r.PathValue("id")+"/rollout?project="+url.QueryEscape(ro.ProjectName), http.StatusSeeOther)
}
