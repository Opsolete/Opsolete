package httpserver

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	Empty   bool               // the source has no templates: nothing to promote
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
	page := &promotePage{Page: workspaceNav(ws.Name, "promote", "", level == app.LevelOperate), Source: ws.Name, To: to, F: f,
		Empty: ws.Templates == 0}
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
		for i, m := range page.P.Modules {
			for j, in := range m.Inputs {
				if in.Field == "" {
					continue
				}
				page.F.Labels[in.Field] = m.Module + ": " + in.Title()
				if in.Sensitive && values[in.Field] != "" { // kept, sealed, for the next try
					page.P.Modules[i].Inputs[j].Form.Draft = s.sealDraft(r, in.Field, values[in.Field])
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
	to, values := promoteTarget(f), promoteValues(f)
	for k := range f { // a refused promotion's typed secrets (drafts.go)
		if field, ok := strings.CutPrefix(k, draftPrefix); ok && values[field] == "" {
			if v := s.draft(r, f, field); v != "" {
				values[field] = v
			}
		}
	}
	p, run, err := s.App.Promote(r.Context(), actor(r), r.PathValue("name"), to, values, f.Get("digest"))
	if s.failed(w, r, err, "promote", f, func(status int, fs *formState) { s.renderPromote(w, r, status, to, values, fs) }) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+p.Target.Name+"/runs/"+run.ID.String(), http.StatusSeeOther)
}

// promoteValues reads a posted promotion's asked-for values by field (<module>.<input>), as formValues reads a
// template form's: a list input's rows (<field>.<row>.<name>) and an object input's fields (<field>.<name>) as
// JSON, rows left wholly empty dropped.
func promoteValues(f url.Values) map[string]string {
	values := map[string]string{}
	rows := map[string]map[int]map[string]string{}
	objects := map[string]map[string]string{}
	for k := range f {
		if promoteFields[k] || strings.HasPrefix(k, draftPrefix) {
			continue
		}
		name := strings.TrimPrefix(k, secretPrefix)
		parts := strings.Split(name, ".")
		switch n, err := strconv.Atoi(parts[min(2, len(parts)-1)]); {
		case len(parts) == 4 && err == nil && n >= 0: // a list's row field
			field := parts[0] + "." + parts[1]
			if rows[field] == nil {
				rows[field] = map[int]map[string]string{}
			}
			if rows[field][n] == nil {
				rows[field][n] = map[string]string{}
			}
			rows[field][n][parts[3]] = f.Get(k)
		case len(parts) == 3: // an object's field
			field := parts[0] + "." + parts[1]
			if objects[field] == nil {
				objects[field] = map[string]string{}
			}
			objects[field][parts[2]] = f.Get(k)
		default:
			values[name] = f.Get(k)
		}
	}
	for field, byRow := range rows {
		out := []map[string]string{}
		for _, n := range slices.Sorted(maps.Keys(byRow)) {
			if slices.ContainsFunc(slices.Collect(maps.Values(byRow[n])), func(v string) bool { return strings.TrimSpace(v) != "" }) {
				out = append(out, byRow[n])
			}
		}
		b, _ := json.Marshal(out)
		values[field] = string(b)
	}
	for field, obj := range objects {
		b, _ := json.Marshal(obj)
		values[field] = string(b)
	}
	return values
}
