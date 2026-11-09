package httpserver

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
)

// Promotion (app.PreviewPromotion, app.Promote): GET /workspaces/{name}/promote picks a target and,
// with ?target=<workspace> or ?new=<name>&project=<project>, previews it; POST promotes it.

// promotePage is the picker (P nil) or a promotion's preview (promote.html).
type promotePage struct {
	Page
	Source  string
	Targets app.PromoteTargets // the picker's choices
	P       *app.Promotion     // the preview; nil: pick a target
	To      app.PromoteTarget  // as requested
	F       *formState
}

// promoteFields are the promotion form's own fields; the others are asked-for inputs.
var promoteFields = map[string]bool{"target": true, "new": true, "project": true, "digest": true}

// promoteTarget reads the target from a query or form.
func promoteTarget(v url.Values) app.PromoteTarget {
	if v.Has("new") {
		return app.PromoteTarget{Name: strings.TrimSpace(v.Get("new")), New: true, Project: v.Get("project")}
	}
	return app.PromoteTarget{Name: v.Get("target")}
}

func (s *Server) promotePage(w http.ResponseWriter, r *http.Request) {
	s.renderPromote(w, r, http.StatusOK, promoteTarget(r.URL.Query()), nil, nil)
}

// renderPromote shows the picker, or the preview of promoting to to with the asked-for values
// (nil: not posted); f: a refused promotion. A target the preview refuses shows the picker with why.
func (s *Server) renderPromote(w http.ResponseWriter, r *http.Request, status int, to app.PromoteTarget, values map[string]string, f *formState) {
	ws, level, ok := s.loadWorkspace(w, r, app.LevelRead)
	if !ok {
		return
	}
	page := &promotePage{Page: workspaceNav(ws.Name, "promote", "", level == app.LevelOperate), Source: ws.Name, To: to, F: f}
	if to.Name != "" || to.New {
		p, err := s.App.PreviewPromotion(r.Context(), actor(r), ws.Name, to, values)
		if err == nil {
			page.P = &p
		} else if fs, st, ok := refused("target", r.URL.Query(), err); ok {
			if f != nil {
				fs.Input = f.Input
			}
			names := map[string]string{fieldWorkspace: "target", fieldProject: "project"}
			if to.New {
				names[fieldWorkspace] = "new"
			}
			for from, name := range names {
				if e, ok := fs.Fields[from]; ok {
					delete(fs.Fields, from)
					fs.Fields[name] = e
				}
			}
			page.F, status = fs, st
		} else {
			s.uiError(w, r, err)
			return
		}
	}
	if page.P == nil {
		var err error
		if page.Targets, err = s.App.PromoteTargets(r.Context(), actor(r), ws.Name); err != nil {
			s.uiError(w, r, err)
			return
		}
	} else if page.F != nil {
		page.F.Labels = map[string]string{}
		for _, m := range page.P.Modules {
			for _, in := range m.Inputs {
				if in.Field != "" {
					page.F.Labels[in.Field] = m.Module + ": " + in.Title()
				}
			}
		}
	}
	s.render(w, r, status, "promote.html", page)
}

// promote promotes the workspace to the target the preview showed, and opens the target's run.
func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	to, values := promoteTarget(f), map[string]string{}
	for k := range f {
		if !promoteFields[k] {
			values[strings.TrimPrefix(k, secretPrefix)] = f.Get(k)
		}
	}
	p, run, err := s.App.Promote(r.Context(), actor(r), r.PathValue("name"), to, values, f.Get("digest"))
	if s.failed(w, r, err, "promote", f, func(status int, fs *formState) { s.renderPromote(w, r, status, to, values, fs) }) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+p.Target.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}
