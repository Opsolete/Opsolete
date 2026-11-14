package httpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// The JSON API (/api/v1): the UI's operations for scripts and other front ends, with the same access rules. A
// client is a signed-in browser (its session; a cross-site POST is refused like the UI's) or carries a user token
// (terraform login) or a service token (automation: admin rights, audited as token:<name>) as a Bearer token. Objects
// are addressed by id: names change, and Terraform keeps ids; lists filter by name (?name=). Errors are {"error": message, "fields": {input: message}} with the UI's
// statuses: 401 not signed in, 403, 404, 409 a conflict, 422 invalid input.

const apiPrefix = "/api/v1"

// maxAPIBody bounds a request body: inputs of one template.
const maxAPIBody = 1 << 20

func (s *Server) apiRoutes(mux routes) {
	p := apiPrefix
	mux.HandleFunc("GET "+p+"/templates", s.apiUser(s.apiTemplates))
	mux.HandleFunc("GET "+p+"/templates/{id}", s.apiUser(s.apiTemplate))
	mux.HandleFunc("GET "+p+"/workspaces", s.apiUser(s.apiWorkspaces))
	mux.HandleFunc("POST "+p+"/workspaces", s.apiUser(s.apiCreateWorkspace))
	ws := func(h http.HandlerFunc) http.HandlerFunc { return s.apiUser(s.workspaceByID(h)) }
	mux.HandleFunc("GET "+p+"/workspaces/{id}", ws(s.apiWorkspace))
	mux.HandleFunc("PATCH "+p+"/workspaces/{id}", ws(s.apiUpdateWorkspace))
	mux.HandleFunc("DELETE "+p+"/workspaces/{id}", ws(s.apiDeleteWorkspace))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/templates", ws(s.apiAddTemplate))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/templates/{module}", ws(s.apiTemplateForm))
	mux.HandleFunc("PUT "+p+"/workspaces/{id}/templates/{module}", ws(s.apiSaveTemplate))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/runs", ws(s.apiRuns))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/runs", ws(s.apiStartRun))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/runs/{run}", ws(s.apiRun))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/runs/{run}/confirm", ws(s.apiDecide(s.App.ConfirmRun)))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/runs/{run}/discard", ws(s.apiDecide(s.App.DiscardRun)))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/runs/{run}/cancel", ws(s.apiCancel))
	s.apiPlatformRoutes(mux)
}

// apiUser admits a token's user or service token (Bearer), else a signed-in user (session). A token sent wins over a
// session: it's what the client chose, also on a dev server (AUTH_DEV_USER) where every request has a session.
func (s *Server) apiUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			if p := auth.CurrentPrincipal(r.Context()); p.Type == "user" {
				next(w, r.WithContext(auth.WithUser(r.Context(), p.User)))
				return
			}
			writeAPIv1Error(w, http.StatusUnauthorized, "sign in, or send a token (terraform login, or a service token) as a Bearer token", nil)
			return
		}
		id, email, role, groups, err := s.App.TokenUser(r.Context(), token)
		if errors.Is(err, storage.ErrNotFound) { // not a user's: a service token acts as itself, an admin
			name, serr := s.App.ServiceTokenName(r.Context(), token)
			if errors.Is(serr, storage.ErrNotFound) {
				writeAPIv1Error(w, http.StatusUnauthorized, "invalid or expired token", nil)
				return
			}
			if serr != nil {
				s.v1Error(w, r, serr)
				return
			}
			a := app.ServiceTokenActor(name)
			u := auth.User{Email: a.Email, Role: "admin"}
			auth.SetPrincipal(r.Context(), u, "token")
			next(w, r.WithContext(auth.WithUser(r.Context(), u)))
			return
		}
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		if role == "admin" { // as cloudUser: admin only while the configuration still says so
			role = s.Auth.Role(email, groups)
		}
		u := auth.User{ID: id, Email: email, Role: role}
		auth.SetPrincipal(r.Context(), u, "token")
		next(w, r.WithContext(auth.WithUser(r.Context(), u)))
	}
}

func writeAPIv1(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeAPIv1Error(w http.ResponseWriter, status int, msg string, fields map[string]string) {
	writeAPIv1(w, status, map[string]any{"error": msg, "fields": fields})
}

// v1Error answers an app error with the UI's statuses.
func (s *Server) v1Error(w http.ResponseWriter, r *http.Request, err error) {
	if ve, ok := errors.AsType[*app.ValidationError](err); ok {
		writeAPIv1Error(w, http.StatusUnprocessableEntity, err.Error(), ve.Fields)
		return
	}
	switch {
	case errors.Is(err, app.ErrNotFound):
		writeAPIv1Error(w, http.StatusNotFound, "not found", nil)
	case errors.Is(err, app.ErrForbidden):
		writeAPIv1Error(w, http.StatusForbidden, err.Error(), nil)
	case app.IsConflict(err):
		writeAPIv1Error(w, http.StatusConflict, err.Error(), nil)
	default:
		s.App.Log.Error("api "+r.Method+" "+r.URL.Path, "err", err)
		writeAPIv1Error(w, http.StatusInternalServerError, "internal error", nil)
	}
}

// bodyFields names a ValidationError's fields as the JSON body does: own (app field → body field), the rest
// template inputs, "inputs.<input>".
func bodyFields(err error, own map[string]string) error {
	ve, ok := errors.AsType[*app.ValidationError](err)
	if !ok {
		return err
	}
	out := &app.ValidationError{Message: ve.Message, Fields: map[string]string{}}
	for k, v := range ve.Fields {
		if name, isOwn := own[k]; isOwn {
			out.Fields[name] = v
		} else {
			out.Fields["inputs."+k] = v
		}
	}
	return out
}

// readJSON decodes the request body into v (at most maxAPIBody, no unknown fields); false: answered 400.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAPIBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeAPIv1Error(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), nil)
		return false
	}
	return true
}

// formRaw turns typed input values (JSON) into the form's strings, as a browser posts them: lists, maps and
// objects as JSON, sensitive values as typed.
func formRaw(in map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		switch x := v.(type) {
		case nil:
		case string:
			out[k] = x
		case []any, map[string]any:
			b, _ := json.Marshal(x)
			out[k] = string(b)
		default:
			out[k] = templates.FormatValue(x)
		}
	}
	return out
}

// API shapes.

type apiTemplate struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Module      string   `json:"module"`
	Ref         string   `json:"ref"`
	Form        *apiForm `json:"form,omitempty"`
}

type apiForm struct {
	Importing bool       `json:"importing,omitempty"`
	LockedBy  string     `json:"locked_by,omitempty"` // the project whose values locked fields show
	Fields    []apiField `json:"fields"`
}

type apiField struct {
	Name      string      `json:"name"`
	Label     string      `json:"label"`
	Type      string      `json:"type"`
	Help      string      `json:"help,omitempty"`
	Value     string      `json:"value"` // as the form shows it: lists and objects as JSON; "" for a sensitive one
	Required  bool        `json:"required,omitempty"`
	Sensitive bool        `json:"sensitive,omitempty"`
	Set       bool        `json:"set,omitempty"` // sensitive: a value is stored, omitted it's kept
	Visible   bool        `json:"visible"`       // show_if holds with the form's values
	ShowIf    any         `json:"show_if,omitempty"`
	Locked    string      `json:"locked,omitempty"` // set by the project (locked_by): this value, read-only
	Pattern   string      `json:"pattern,omitempty"`
	Options   []apiOption `json:"options,omitempty"`
	Fields    []apiField  `json:"fields,omitempty"` // a list's row fields, an object's fields
}

type apiOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func toAPIForm(f app.Form) *apiForm {
	out := &apiForm{Importing: f.Importing, LockedBy: f.LockedBy, Fields: []apiField{}}
	for _, fd := range f.Fields {
		af := apiInput(fd.Input)
		af.Label, af.Value, af.Visible, af.Locked, af.Set = fd.Label, fd.Value, fd.Visible, fd.Locked, fd.Set
		out.Fields = append(out.Fields, af)
	}
	return out
}

func apiInput(in templates.Input) apiField {
	af := apiField{Name: in.Name, Label: in.Title(), Type: in.Type, Help: in.Help, Required: in.Required, Sensitive: in.Sensitive,
		Pattern: in.Pattern, Visible: true}
	if in.ShowIf != nil {
		af.ShowIf = in.ShowIf
	}
	for _, o := range in.Options {
		af.Options = append(af.Options, apiOption{Value: o.Value, Label: o.Text()})
	}
	for _, f := range subFields(in) {
		af.Fields = append(af.Fields, apiInput(f))
	}
	return af
}

// subFields are a list's row fields or an object's fields.
func subFields(in templates.Input) []templates.Input {
	if rf := in.RowFields(); rf != nil {
		return rf
	}
	return in.Fields
}

type apiWorkspace struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	ProjectID      string             `json:"project_id"`
	Project        string             `json:"project"` // its name
	AutoApply      bool               `json:"auto_apply"`
	ExecutionMode  string             `json:"execution_mode"`
	DriftDetection bool               `json:"drift_detection"`
	Drifted        bool               `json:"drifted,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	Latest         *apiRun            `json:"latest_run,omitempty"`
	Templates      []apiAddedTemplate `json:"templates,omitempty"`
}

type apiAddedTemplate struct {
	Module   string                  `json:"module"`
	Template string                  `json:"template"`
	Source   string                  `json:"source"`
	Ref      string                  `json:"ref"`
	Inputs   []templates.StoredValue `json:"inputs"`
}

func toAPIWorkspace(w storage.Workspace) apiWorkspace {
	out := apiWorkspace{ID: w.ID.String(), Name: w.Name, ProjectID: w.ProjectID.String(), Project: w.ProjectName, AutoApply: w.AutoApply,
		ExecutionMode: w.ExecutionMode, DriftDetection: w.DriftDetection, Drifted: w.Drifted, CreatedAt: w.CreatedAt}
	if w.Latest != nil {
		r := toAPIRun(*w.Latest)
		out.Latest = &r
	}
	return out
}

type apiRun struct {
	ID            string                   `json:"id"`
	Action        string                   `json:"action"`
	Status        string                   `json:"status"`
	ConfirmReason string                   `json:"confirm_reason,omitempty"`
	CreatedBy     string                   `json:"created_by,omitempty"`
	ConfirmedBy   string                   `json:"confirmed_by,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
	Plan          *runnerapi.Plan          `json:"plan,omitempty"`
	Outputs       map[string]any           `json:"outputs,omitempty"`
	History       []apiStatus              `json:"history,omitempty"`
	Diff          []runnerapi.ResourceDiff `json:"diff,omitempty"`
}

type apiStatus struct {
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

func toAPIRun(r storage.Run) apiRun {
	return apiRun{ID: r.ID.String(), Action: r.Action, Status: r.Status, ConfirmReason: r.ConfirmReason, CreatedBy: r.CreatedBy,
		ConfirmedBy: r.ConfirmedBy, CreatedAt: r.CreatedAt, Plan: r.Plan, Outputs: r.Outputs}
}

// Handlers.

func (s *Server) apiTemplates(w http.ResponseWriter, r *http.Request) {
	out := []apiTemplate{}
	for _, t := range s.App.Templates.List() {
		out = append(out, apiTemplate{ID: t.ID, Name: t.Name, Description: t.Description, Module: t.Module.Source, Ref: t.Module.Ref})
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiTemplate is a template with its deploy form; ?project= locks the inputs that project sets.
func (s *Server) apiTemplate(w http.ResponseWriter, r *http.Request) {
	t, err := s.App.Template(r.Context(), r.PathValue("id"))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	vals, _ := app.FormDefaults(t, nil)
	state := app.FormState{Values: vals}
	if name := r.URL.Query().Get("project"); name != "" {
		p, err := s.App.Project(r.Context(), actor(r), name)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		env, err := s.App.ProjectEnvironment(r.Context(), p.ID)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		state.Locked, state.Project = env.Locked(t, nil, nil), p.Name
	}
	writeAPIv1(w, http.StatusOK, apiTemplate{ID: t.ID, Name: t.Name, Description: t.Description, Module: t.Module.Source, Ref: t.Module.Ref,
		Form: toAPIForm(app.NewForm(t, state))})
}

// apiWorkspaces lists the workspaces the actor sees; ?project_id= and ?name= filter.
func (s *Server) apiWorkspaces(w http.ResponseWriter, r *http.Request) {
	project := ""
	if id := r.URL.Query().Get("project_id"); id != "" {
		p, ok := s.apiProject(w, r, id)
		if !ok {
			return
		}
		project = p.Name
	}
	list, err := s.App.ListWorkspaces(r.Context(), actor(r), project)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiWorkspace{}
	for _, ws := range list {
		if name := r.URL.Query().Get("name"); name == "" || ws.Name == name {
			out = append(out, toAPIWorkspace(ws))
		}
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiWorkspace is a workspace with its templates and their inputs as its Overview lists them (sensitive values
// never, the project's values in place).
func (s *Server) apiWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, _, err := s.App.Workspace(r.Context(), actor(r), r.PathValue("name"), app.LevelRead)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	added, err := s.App.Store.WorkspaceTemplates(r.Context(), ws.ID)
	if err == nil {
		var env app.EnvironmentMap
		env, err = s.App.ProjectEnvironment(r.Context(), ws.ProjectID)
		out := toAPIWorkspace(ws)
		for _, a := range added {
			t, _, terr := s.App.TemplateAt(r.Context(), a.TemplateID, a.ModuleSource, a.ModuleRef)
			err = cmp.Or(err, terr)
			out.Templates = append(out.Templates, apiAddedTemplate{Module: a.Name, Template: a.TemplateID, Source: a.ModuleSource, Ref: a.ModuleRef,
				Inputs: env.Describe(t, a.Inputs, a.Follows, ws.ProjectName)})
		}
		if err == nil {
			writeAPIv1(w, http.StatusOK, out)
			return
		}
	}
	s.v1Error(w, r, err)
}

type apiNewWorkspace struct {
	Name           string         `json:"name"`
	ProjectID      string         `json:"project_id"` // "": the default project
	Template       string         `json:"template"`   // "": an empty workspace
	AutoApply      bool           `json:"auto_apply"`
	ExecutionMode  string         `json:"execution_mode"` // an empty workspace's: "" is remote
	DriftDetection bool           `json:"drift_detection"`
	Inputs         map[string]any `json:"inputs"`
}

// apiCreateWorkspace creates a workspace: from a template, queueing its first plan (201 with the run), or empty.
func (s *Server) apiCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var in apiNewWorkspace
	if !readJSON(w, r, &in) {
		return
	}
	project := ""
	if in.ProjectID != "" {
		p, ok := s.apiProject(w, r, in.ProjectID)
		if !ok {
			return
		}
		project = p.Name
	}
	if in.Template == "" {
		s.apiCreateEmptyWorkspace(w, r, in, project)
		return
	}
	ws, run, err := s.App.CreateWorkspace(r.Context(), actor(r), app.NewWorkspace{Name: strings.TrimSpace(in.Name), Project: project,
		AutoApply: in.AutoApply, TemplateID: in.Template, Inputs: formRaw(in.Inputs)})
	if err != nil {
		s.v1Error(w, r, bodyFields(err, map[string]string{app.FieldName: "name", app.FieldProject: "project_id"}))
		return
	}
	out := toAPIWorkspace(ws)
	rr := toAPIRun(run)
	out.Latest = &rr
	writeAPIv1(w, http.StatusCreated, out)
}

type apiTemplateInputs struct {
	Template string         `json:"template,omitempty"` // adding: the template
	Inputs   map[string]any `json:"inputs"`             // sensitive ones omitted keep their stored value
}

// apiAddTemplate adds a template to a workspace and queues its plan: 201 with the module name and the run.
func (s *Server) apiAddTemplate(w http.ResponseWriter, r *http.Request) {
	var in apiTemplateInputs
	if !readJSON(w, r, &in) {
		return
	}
	s.apiSave(w, r, "", in, http.StatusCreated)
}

// apiSaveTemplate saves an added template's inputs (and the template's current module version) and queues the plan.
func (s *Server) apiSaveTemplate(w http.ResponseWriter, r *http.Request) {
	var in apiTemplateInputs
	if !readJSON(w, r, &in) {
		return
	}
	s.apiSave(w, r, r.PathValue("module"), in, http.StatusOK)
}

func (s *Server) apiSave(w http.ResponseWriter, r *http.Request, module string, in apiTemplateInputs, status int) {
	module, run, err := s.App.SaveTemplate(r.Context(), actor(r), r.PathValue("name"), module, in.Template, formRaw(in.Inputs), nil, "")
	if err != nil {
		s.v1Error(w, r, bodyFields(err, nil))
		return
	}
	writeAPIv1(w, status, map[string]any{"module": module, "run": toAPIRun(run)})
}

// apiTemplateForm is an added template's configure form: its stored values (sensitive ones never), the project's in place.
func (s *Server) apiTemplateForm(w http.ResponseWriter, r *http.Request) {
	f, err := s.App.TemplateForm(r.Context(), actor(r), r.PathValue("name"), r.PathValue("module"), "")
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	env, err := s.App.ProjectEnvironment(r.Context(), f.Workspace.ProjectID)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	vals, set := app.FormDefaults(f.Template, f.Current.Inputs)
	form := app.NewForm(f.Template, app.FormState{Values: vals, Set: set, Project: f.Workspace.ProjectName,
		Locked: env.LockedForm(f.Template, f.Current.Inputs, f.Current.Follows, nil)})
	writeAPIv1(w, http.StatusOK, map[string]any{"module": f.Current.Name, "template": f.Template.ID, "ref": f.Current.ModuleRef,
		"latest_ref": f.Template.Module.Ref, "form": toAPIForm(form)})
}

func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	ws, _, err := s.App.Workspace(r.Context(), actor(r), r.PathValue("name"), app.LevelRead)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	runs, err := s.App.Store.Runs(r.Context(), ws.ID, 25)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiRun{}
	for _, run := range runs {
		out = append(out, toAPIRun(run))
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiRun is a run with its status history and its plan's attribute changes (read access, like its log).
func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r, "run") // not an id: not found
	d, err := s.App.Run(r.Context(), actor(r), r.PathValue("name"), id, app.LevelRead)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := toAPIRun(d.Run)
	for _, h := range d.History {
		st := apiStatus{Status: h.Status, At: h.CreatedAt}
		if h.Message != nil {
			st.Message = *h.Message
		}
		out.History = append(out.History, st)
	}
	if out.Diff, err = s.App.RunDiff(r.Context(), actor(r), d.Workspace.Name, d.Run.ID); err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, out)
}

type apiNewRun struct {
	Action  string   `json:"action"`
	Replace []string `json:"replace,omitempty"`
}

func (s *Server) apiStartRun(w http.ResponseWriter, r *http.Request) {
	var in apiNewRun
	if !readJSON(w, r, &in) {
		return
	}
	_, run, err := s.App.StartRun(r.Context(), actor(r), r.PathValue("name"), app.NewRun{Action: in.Action, Replace: slices.Clone(in.Replace)})
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusCreated, toAPIRun(run))
}

// apiDecide confirms or discards a plan that waits: 204.
func (s *Server) apiDecide(decide func(ctx context.Context, actor app.Actor, workspace string, id uuid.UUID) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r, "run") // not an id: not found
		if err := decide(r.Context(), actor(r), r.PathValue("name"), id); err != nil {
			s.v1Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type apiCancelRun struct {
	Force bool `json:"force"`
}

// apiCancel cancels a run (force: at once, fencing its runner off): 200 with its status.
func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	var in apiCancelRun
	if !readJSON(w, r, &in) {
		return
	}
	id, _ := pathID(r, "run") // not an id: not found
	status, err := s.App.CancelRun(r.Context(), actor(r), r.PathValue("name"), id, in.Force)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, map[string]string{"status": status})
}
