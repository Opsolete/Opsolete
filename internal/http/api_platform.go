package httpserver

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/storage"
)

// The platform objects of the JSON API, what a Terraform provider manages (docs/terraform-provider.md): projects
// and their environment values, workspaces' settings and variables, variable sets (their variables and where they
// apply) and VCS connections. Each handler is the UI form's app function with the same access rules; objects are
// addressed by id (resolved to the name the app functions take), and PATCH changes only the fields sent.

func (s *Server) apiPlatformRoutes(mux routes) {
	p := apiPrefix
	u := s.apiUser
	proj := func(h http.HandlerFunc) http.HandlerFunc { return u(s.projectByID(h)) }
	set := func(h http.HandlerFunc) http.HandlerFunc { return u(s.setByID(h)) }
	ws := func(h http.HandlerFunc) http.HandlerFunc { return u(s.workspaceByID(h)) }
	mux.HandleFunc("GET "+p+"/projects", u(s.apiProjects))
	mux.HandleFunc("POST "+p+"/projects", u(s.apiCreateProject))
	mux.HandleFunc("GET "+p+"/projects/{id}", proj(s.apiGetProject))
	mux.HandleFunc("PATCH "+p+"/projects/{id}", proj(s.apiUpdateProject))
	mux.HandleFunc("DELETE "+p+"/projects/{id}", proj(s.apiDeleteProject))
	mux.HandleFunc("GET "+p+"/projects/{id}/environment", proj(s.apiEnvironment))
	mux.HandleFunc("PUT "+p+"/projects/{id}/environment/{template}/{input}", proj(s.apiSetEnvironmentValue))
	mux.HandleFunc("DELETE "+p+"/projects/{id}/environment/{template}/{input}", proj(s.apiClearEnvironmentValue))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/variables", ws(s.apiVariables(false)))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/variables", ws(s.apiCreateVariable(false)))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/variables/{variable}", ws(s.apiGetVariable(false)))
	mux.HandleFunc("PATCH "+p+"/workspaces/{id}/variables/{variable}", ws(s.apiUpdateVariable(false)))
	mux.HandleFunc("DELETE "+p+"/workspaces/{id}/variables/{variable}", ws(s.apiDeleteVariable(false)))
	mux.HandleFunc("GET "+p+"/variable-sets", u(s.apiVariableSets))
	mux.HandleFunc("POST "+p+"/variable-sets", u(s.apiCreateVariableSet))
	mux.HandleFunc("GET "+p+"/variable-sets/{id}", set(s.apiGetVariableSet))
	mux.HandleFunc("PATCH "+p+"/variable-sets/{id}", set(s.apiUpdateVariableSet))
	mux.HandleFunc("DELETE "+p+"/variable-sets/{id}", set(s.apiDeleteVariableSet))
	mux.HandleFunc("GET "+p+"/variable-sets/{id}/variables", set(s.apiVariables(true)))
	mux.HandleFunc("POST "+p+"/variable-sets/{id}/variables", set(s.apiCreateVariable(true)))
	mux.HandleFunc("GET "+p+"/variable-sets/{id}/variables/{variable}", set(s.apiGetVariable(true)))
	mux.HandleFunc("PATCH "+p+"/variable-sets/{id}/variables/{variable}", set(s.apiUpdateVariable(true)))
	mux.HandleFunc("DELETE "+p+"/variable-sets/{id}/variables/{variable}", set(s.apiDeleteVariable(true)))
	mux.HandleFunc("PUT "+p+"/variable-sets/{id}/projects/{target}", set(s.apiAttachSet(true, true)))
	mux.HandleFunc("DELETE "+p+"/variable-sets/{id}/projects/{target}", set(s.apiAttachSet(true, false)))
	mux.HandleFunc("PUT "+p+"/variable-sets/{id}/workspaces/{target}", set(s.apiAttachSet(false, true)))
	mux.HandleFunc("DELETE "+p+"/variable-sets/{id}/workspaces/{target}", set(s.apiAttachSet(false, false)))
	mux.HandleFunc("GET "+p+"/vcs-connections", u(s.apiVCSConnections))
	mux.HandleFunc("POST "+p+"/vcs-connections", u(s.apiCreateVCSConnection))
	mux.HandleFunc("GET "+p+"/vcs-connections/{id}", u(s.apiGetVCSConnection))
	mux.HandleFunc("PATCH "+p+"/vcs-connections/{id}", u(s.apiUpdateVCSConnection))
	mux.HandleFunc("DELETE "+p+"/vcs-connections/{id}", u(s.apiDeleteVCSConnection))
}

// apiPathID is path value name as an id; a malformed one is answered 404, as an unknown one.
func apiPathID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeAPIv1Error(w, http.StatusNotFound, "not found", nil)
	}
	return id, err == nil
}

// workspaceByID resolves {id} to the workspace's name ({name}, what the handlers and app functions take); access is
// theirs to check, and an unknown id is 404 like a workspace the actor may not see.
func (s *Server) workspaceByID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := apiPathID(w, r, "id")
		if !ok {
			return
		}
		ws, err := s.App.Store.GetWorkspaceByID(r.Context(), id)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		r.SetPathValue("name", ws.Name)
		next(w, r)
	}
}

// projectByID resolves {id} to a project the actor sees ({project}: its name).
func (s *Server) projectByID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := s.apiProject(w, r, r.PathValue("id")); ok {
			r.SetPathValue("project", p.Name)
			next(w, r)
		}
	}
}

// apiProject is the project of id the actor sees; it answered the request otherwise.
func (s *Server) apiProject(w http.ResponseWriter, r *http.Request, id string) (storage.Project, bool) {
	pid, err := uuid.Parse(id)
	if err != nil {
		writeAPIv1Error(w, http.StatusNotFound, "not found", nil)
		return storage.Project{}, false
	}
	p, err := s.App.Store.ProjectByID(r.Context(), pid)
	if err == nil {
		p, err = s.App.Project(r.Context(), actor(r), p.Name)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return storage.Project{}, false
	}
	return p, true
}

// setByID resolves {id} to a variable set ({set}: its name). Sets are admins': the app functions refuse others.
func (s *Server) setByID(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := apiPathID(w, r, "id")
		if !ok {
			return
		}
		if !actor(r).Admin { // before the lookup: an id says nothing to others
			writeAPIv1Error(w, http.StatusForbidden, "variable sets are admins'", nil)
			return
		}
		vs, err := s.App.Store.VariableSetByID(r.Context(), id)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		r.SetPathValue("set", vs.Name)
		next(w, r)
	}
}

// Projects and their environment values.

type apiProjectJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	RunnerPool  string `json:"runner_pool"`
	Default     bool   `json:"default"`
}

func toAPIProject(p storage.Project) apiProjectJSON {
	return apiProjectJSON{ID: p.ID.String(), Name: p.Name, Description: p.Description, RunnerPool: p.RunnerPool, Default: p.Default}
}

// apiProjects lists the projects the actor sees; ?name= filters.
func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Projects(r.Context(), actor(r))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiProjectJSON{}
	for _, p := range list {
		if name := r.URL.Query().Get("name"); name == "" || p.Name == name {
			out = append(out, toAPIProject(p))
		}
	}
	writeAPIv1(w, http.StatusOK, out)
}

type apiProjectInput struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	RunnerPool  *string `json:"runner_pool"`
	Default     *bool   `json:"default"` // true makes it the default project; false is refused while it is (pick another)
}

// apiCreateProject creates a project (admins): 201 with it.
func (s *Server) apiCreateProject(w http.ResponseWriter, r *http.Request) {
	var in apiProjectInput
	if !readJSON(w, r, &in) {
		return
	}
	p, err := s.App.CreateProject(r.Context(), actor(r), strings.TrimSpace(deref(in.Name)), deref(in.Description))
	if err == nil && (in.RunnerPool != nil || deref(in.Default)) {
		in.Name, in.Description = nil, nil
		p, err = s.updateProject(r, p, in)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusCreated, toAPIProject(p))
}

func (s *Server) apiGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.App.Project(r.Context(), actor(r), r.PathValue("project"))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, toAPIProject(p))
}

// apiUpdateProject changes the fields sent (admins): 200 with the project.
func (s *Server) apiUpdateProject(w http.ResponseWriter, r *http.Request) {
	var in apiProjectInput
	if !readJSON(w, r, &in) {
		return
	}
	p, err := s.App.Project(r.Context(), actor(r), r.PathValue("project"))
	if err == nil {
		p, err = s.updateProject(r, p, in)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, toAPIProject(p))
}

// updateProject saves p with the fields of in that are set, and returns it as saved.
func (s *Server) updateProject(r *http.Request, p storage.Project, in apiProjectInput) (storage.Project, error) {
	if in.Default != nil && !*in.Default && p.Default {
		return p, &app.ValidationError{Fields: map[string]string{"default": "the default project can't be unset: make another project the default"}}
	}
	set := app.ProjectSettings{Name: strings.TrimSpace(cmp.Or(deref(in.Name), p.Name)), Description: p.Description, RunnerPool: p.RunnerPool,
		Default: in.Default != nil && *in.Default && !p.Default}
	if in.Description != nil {
		set.Description = *in.Description
	}
	if in.RunnerPool != nil {
		set.RunnerPool = *in.RunnerPool
	}
	if err := s.App.UpdateProject(r.Context(), actor(r), p.Name, set); err != nil {
		return p, err
	}
	return s.App.Store.ProjectByID(r.Context(), p.ID)
}

// apiDeleteProject deletes an empty project (admins): 204.
func (s *Server) apiDeleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteProject(r.Context(), actor(r), r.PathValue("project")); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiEnvironmentValue struct {
	TemplateID string     `json:"template_id"`
	Input      string     `json:"input"`
	Value      string     `json:"value"` // canonical: "7", "true", an option's value
	KeepOwn    bool       `json:"keep_own"`
	SetBy      string     `json:"set_by,omitempty"`
	SetAt      *time.Time `json:"set_at,omitempty"`
}

// apiEnvironment lists the project's environment values (anyone who sees the project): the inputs it sets.
func (s *Server) apiEnvironment(w http.ResponseWriter, r *http.Request) {
	ev, err := s.App.EnvironmentValues(r.Context(), actor(r), r.PathValue("project"))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiEnvironmentValue{}
	for _, t := range ev.Templates {
		for _, in := range t.Inputs {
			if in.Set {
				at := in.UpdatedAt
				out = append(out, apiEnvironmentValue{TemplateID: t.Template.ID, Input: in.Name, Value: in.Value, KeepOwn: in.KeepOwn, SetBy: in.UpdatedBy, SetAt: &at})
			}
		}
	}
	writeAPIv1(w, http.StatusOK, out)
}

// apiSetEnvironmentValue sets the project's value for a template's per-environment input (admins and operators of
// the project): 200 with it. The input's own rules check the value (422 on "value").
func (s *Server) apiSetEnvironmentValue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Value   string `json:"value"`
		KeepOwn bool   `json:"keep_own"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	tpl, input := r.PathValue("template"), r.PathValue("input")
	if err := s.App.SetEnvironmentValue(r.Context(), actor(r), r.PathValue("project"), tpl, input, in.Value, in.KeepOwn); err != nil {
		s.v1Error(w, r, err)
		return
	}
	ev, err := s.App.EnvironmentValues(r.Context(), actor(r), r.PathValue("project"))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	for _, t := range ev.Templates {
		for _, ei := range t.Inputs {
			if t.Template.ID == tpl && ei.Name == input {
				at := ei.UpdatedAt
				writeAPIv1(w, http.StatusOK, apiEnvironmentValue{TemplateID: tpl, Input: input, Value: ei.Value, KeepOwn: ei.KeepOwn, SetBy: ei.UpdatedBy, SetAt: &at})
				return
			}
		}
	}
	s.v1Error(w, r, app.ErrNotFound)
}

// apiClearEnvironmentValue removes the project's value: its workspaces use their own again. 204.
func (s *Server) apiClearEnvironmentValue(w http.ResponseWriter, r *http.Request) {
	if err := s.App.ClearEnvironmentValue(r.Context(), actor(r), r.PathValue("project"), r.PathValue("template"), r.PathValue("input")); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Workspaces: an empty one, its settings, deleting it.

func (s *Server) apiCreateEmptyWorkspace(w http.ResponseWriter, r *http.Request, in apiNewWorkspace, project string) {
	ws, err := s.App.CreateEmptyWorkspace(r.Context(), actor(r), app.EmptyWorkspace{Name: strings.TrimSpace(in.Name), Project: project,
		AutoApply: in.AutoApply, ExecutionMode: in.ExecutionMode})
	if err == nil && in.DriftDetection {
		err = s.App.SaveHealthSettings(r.Context(), actor(r), ws.Name, true)
		ws.DriftDetection = err == nil
	}
	if err != nil {
		s.v1Error(w, r, bodyFields(err, map[string]string{app.FieldName: "name", app.FieldProject: "project_id"}))
		return
	}
	writeAPIv1(w, http.StatusCreated, toAPIWorkspace(ws))
}

// apiUpdateWorkspace changes the settings sent (operate; moving it needs operate on the target project too): 200.
func (s *Server) apiUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name           *string `json:"name"`
		ProjectID      *string `json:"project_id"`
		AutoApply      *bool   `json:"auto_apply"`
		ExecutionMode  *string `json:"execution_mode"`
		DriftDetection *bool   `json:"drift_detection"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx, a := r.Context(), actor(r)
	ws, _, err := s.App.Workspace(ctx, a, r.PathValue("name"), app.LevelOperate)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	if in.Name != nil || in.AutoApply != nil || in.ExecutionMode != nil {
		g := app.GeneralSettings{Name: strings.TrimSpace(cmp.Or(deref(in.Name), ws.Name)), ExecutionMode: cmp.Or(deref(in.ExecutionMode), ws.ExecutionMode),
			AutoApply: ws.AutoApply}
		if in.AutoApply != nil {
			g.AutoApply = *in.AutoApply
		}
		if ws, err = s.App.SaveGeneralSettings(ctx, a, ws.Name, g); err != nil {
			s.v1Error(w, r, bodyFields(err, map[string]string{app.FieldName: "name"}))
			return
		}
	}
	if in.DriftDetection != nil && *in.DriftDetection != ws.DriftDetection {
		if err := s.App.SaveHealthSettings(ctx, a, ws.Name, *in.DriftDetection); err != nil {
			s.v1Error(w, r, err)
			return
		}
	}
	if in.ProjectID != nil && *in.ProjectID != ws.ProjectID.String() {
		p, ok := s.apiProject(w, r, *in.ProjectID)
		if !ok {
			return
		}
		if err := s.App.MoveWorkspace(ctx, a, ws.Name, p.Name); err != nil {
			s.v1Error(w, r, err)
			return
		}
	}
	if ws, _, err = s.App.Workspace(ctx, a, ws.Name, app.LevelRead); err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, http.StatusOK, toAPIWorkspace(ws))
}

// apiDeleteWorkspace deletes the workspace (operate): refused while a run is active or its state manages resources.
// Never forced here: a workspace's state is Opsolete's alone, so throwing one away that still manages resources is
// the UI's admin action (Delete workspace anyway), not the API's. 204.
func (s *Server) apiDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteWorkspace(r.Context(), actor(r), r.PathValue("name"), false); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Variables: a workspace's own (set false; operate to change, read to list) or a variable set's (set true; admins).

type apiVariable struct {
	ID        string  `json:"id"`
	Category  string  `json:"category"`
	Key       string  `json:"key"`
	Value     *string `json:"value"` // null when sensitive: write-only
	Sensitive bool    `json:"sensitive"`
	HCL       bool    `json:"hcl"`
}

func toAPIVariable(v app.Variable) apiVariable {
	out := apiVariable{ID: v.ID.String(), Category: v.Category, Key: v.Key, Sensitive: v.Sensitive, HCL: v.HCL}
	if !v.Sensitive {
		out.Value = &v.Value
	}
	return out
}

// variablesOf lists the owner's variables: the workspace's own, or the set's.
func (s *Server) variablesOf(r *http.Request, set bool) ([]app.Variable, error) {
	if set {
		sv, err := s.App.VariableSet(r.Context(), actor(r), r.PathValue("set"))
		return sv.Vars, err
	}
	wv, err := s.App.WorkspaceVariables(r.Context(), actor(r), r.PathValue("name"))
	return wv.Own, err
}

func (s *Server) setVariable(r *http.Request, set bool, in app.VariableInput) error {
	if set {
		return s.App.SetSetVariable(r.Context(), actor(r), r.PathValue("set"), in)
	}
	return s.App.SetWorkspaceVariable(r.Context(), actor(r), r.PathValue("name"), in)
}

// variableBy finds the owner's variable by match; ErrNotFound.
func (s *Server) variableBy(r *http.Request, set bool, match func(app.Variable) bool) (app.Variable, error) {
	vars, err := s.variablesOf(r, set)
	if err != nil {
		return app.Variable{}, err
	}
	if i := slices.IndexFunc(vars, match); i >= 0 {
		return vars[i], nil
	}
	return app.Variable{}, app.ErrNotFound
}

func (s *Server) apiVariables(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		vars, err := s.variablesOf(r, set)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		out := []apiVariable{}
		for _, v := range vars {
			out = append(out, toAPIVariable(v))
		}
		writeAPIv1(w, http.StatusOK, out)
	}
}

type apiVariableInput struct {
	Category  *string `json:"category"`
	Key       *string `json:"key"`
	Value     *string `json:"value"`
	Sensitive *bool   `json:"sensitive"`
	HCL       *bool   `json:"hcl"`
}

// apiCreateVariable creates a variable: 201 with it; a variable of that category and key exists: 409.
func (s *Server) apiCreateVariable(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in apiVariableInput
		if !readJSON(w, r, &in) {
			return
		}
		cat, key := deref(in.Category), strings.TrimSpace(deref(in.Key))
		same := func(v app.Variable) bool { return v.Category == cat && v.Key == key }
		if _, err := s.variableBy(r, set, same); !errors.Is(err, app.ErrNotFound) {
			if err == nil {
				writeAPIv1Error(w, http.StatusConflict, "a variable "+key+" of category "+cat+" exists: change that one", nil)
			} else {
				s.v1Error(w, r, err)
			}
			return
		}
		err := s.setVariable(r, set, app.VariableInput{Category: cat, Key: key, Value: deref(in.Value), Sensitive: deref(in.Sensitive), HCL: deref(in.HCL)})
		v, ferr := s.variableBy(r, set, same)
		if err = cmp.Or(err, ferr); err != nil {
			s.v1Error(w, r, err)
			return
		}
		writeAPIv1(w, http.StatusCreated, toAPIVariable(v))
	}
}

func (s *Server) apiGetVariable(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.variableBy(r, set, func(v app.Variable) bool { return v.ID.String() == r.PathValue("variable") })
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		writeAPIv1(w, http.StatusOK, toAPIVariable(v))
	}
}

// apiUpdateVariable changes a variable's value, sensitivity or HCL flag: 200. Its category and key are fixed (422:
// replace it). A sensitive value can't be read back, so changing a sensitive variable sends its value.
func (s *Server) apiUpdateVariable(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in apiVariableInput
		if !readJSON(w, r, &in) {
			return
		}
		v, err := s.variableBy(r, set, func(v app.Variable) bool { return v.ID.String() == r.PathValue("variable") })
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		fields := map[string]string{}
		if in.Category != nil && *in.Category != v.Category {
			fields["category"] = "can't change: delete the variable and create another"
		}
		if in.Key != nil && strings.TrimSpace(*in.Key) != v.Key {
			fields["key"] = "can't change: delete the variable and create another"
		}
		if in.Value == nil && v.Sensitive {
			fields["value"] = "a sensitive value can't be read back: send it"
		}
		if len(fields) > 0 {
			s.v1Error(w, r, &app.ValidationError{Fields: fields})
			return
		}
		next := app.VariableInput{Category: v.Category, Key: v.Key, Value: cmp.Or(deref(in.Value), v.Value), Sensitive: v.Sensitive, HCL: v.HCL}
		if in.Value != nil {
			next.Value = *in.Value
		}
		if in.Sensitive != nil {
			next.Sensitive = *in.Sensitive
		}
		if in.HCL != nil {
			next.HCL = *in.HCL
		}
		err = s.setVariable(r, set, next)
		if err == nil {
			v, err = s.variableBy(r, set, func(x app.Variable) bool { return x.ID == v.ID })
		}
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		writeAPIv1(w, http.StatusOK, toAPIVariable(v))
	}
}

func (s *Server) apiDeleteVariable(set bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := apiPathID(w, r, "variable")
		if !ok {
			return
		}
		if _, err := s.variableBy(r, set, func(v app.Variable) bool { return v.ID == id }); err != nil { // 404 for an unknown one
			s.v1Error(w, r, err)
			return
		}
		var err error
		if set {
			err = s.App.DeleteSetVariable(r.Context(), actor(r), r.PathValue("set"), id)
		} else {
			err = s.App.DeleteWorkspaceVariable(r.Context(), actor(r), r.PathValue("name"), id)
		}
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// Variable sets (admins).

type apiVariableSetJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Global       bool     `json:"global"`
	Priority     bool     `json:"priority"`
	ProjectIDs   []string `json:"project_ids"`
	WorkspaceIDs []string `json:"workspace_ids"`
}

func (s *Server) toAPIVariableSet(r *http.Request, vs storage.VariableSet) (apiVariableSetJSON, error) {
	out := apiVariableSetJSON{ID: vs.ID.String(), Name: vs.Name, Description: vs.Description, Global: vs.Global, Priority: vs.Priority,
		ProjectIDs: []string{}, WorkspaceIDs: []string{}}
	var err error
	if out.ProjectIDs, err = s.projectIDs(r, vs.Projects); err == nil {
		out.WorkspaceIDs, err = s.workspaceIDs(r, vs.Workspaces)
	}
	return out, err
}

func (s *Server) projectIDs(r *http.Request, names []string) ([]string, error) {
	out := []string{}
	for _, n := range names {
		p, err := s.App.Store.Project(r.Context(), n)
		if err != nil {
			return nil, err
		}
		out = append(out, p.ID.String())
	}
	return out, nil
}

func (s *Server) workspaceIDs(r *http.Request, names []string) ([]string, error) {
	out := []string{}
	for _, n := range names {
		ws, err := s.App.Store.GetWorkspace(r.Context(), n)
		if err != nil {
			return nil, err
		}
		out = append(out, ws.ID.String())
	}
	return out, nil
}

// apiVariableSets lists the variable sets; ?name= filters.
func (s *Server) apiVariableSets(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.VariableSets(r.Context(), actor(r))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiVariableSetJSON{}
	for _, vs := range list {
		if name := r.URL.Query().Get("name"); name != "" && vs.Name != name {
			continue
		}
		j, err := s.toAPIVariableSet(r, vs)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeAPIv1(w, http.StatusOK, out)
}

type apiVariableSetInput struct {
	Name        *string `json:"name"` // fixed once created
	Description *string `json:"description"`
	Global      *bool   `json:"global"`
	Priority    *bool   `json:"priority"`
}

// apiCreateVariableSet creates a variable set: 201 with it.
func (s *Server) apiCreateVariableSet(w http.ResponseWriter, r *http.Request) {
	var in apiVariableSetInput
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(deref(in.Name))
	err := s.App.CreateVariableSet(r.Context(), actor(r), app.NewVariableSet{Name: name, Description: deref(in.Description), Global: deref(in.Global)})
	if err == nil && deref(in.Priority) {
		err = s.App.UpdateVariableSet(r.Context(), actor(r), name, deref(in.Description), deref(in.Global), true)
	}
	s.writeVariableSet(w, r, name, http.StatusCreated, err)
}

func (s *Server) apiGetVariableSet(w http.ResponseWriter, r *http.Request) {
	s.writeVariableSet(w, r, r.PathValue("set"), http.StatusOK, nil)
}

// apiUpdateVariableSet changes the fields sent: 200. The name is fixed (422: replace it).
func (s *Server) apiUpdateVariableSet(w http.ResponseWriter, r *http.Request) {
	var in apiVariableSetInput
	if !readJSON(w, r, &in) {
		return
	}
	name := r.PathValue("set")
	if in.Name != nil && strings.TrimSpace(*in.Name) != name {
		s.v1Error(w, r, &app.ValidationError{Fields: map[string]string{"name": "can't change: create another set"}})
		return
	}
	sv, err := s.App.VariableSet(r.Context(), actor(r), name)
	if err == nil {
		desc, global, prio := sv.Description, sv.Global, sv.Priority
		if in.Description != nil {
			desc = *in.Description
		}
		if in.Global != nil {
			global = *in.Global
		}
		if in.Priority != nil {
			prio = *in.Priority
		}
		err = s.App.UpdateVariableSet(r.Context(), actor(r), name, desc, global, prio)
	}
	s.writeVariableSet(w, r, name, http.StatusOK, err)
}

// writeVariableSet answers with set name as it is now, or with err.
func (s *Server) writeVariableSet(w http.ResponseWriter, r *http.Request, name string, status int, err error) {
	var sv app.SetVariables
	if err == nil {
		sv, err = s.App.VariableSet(r.Context(), actor(r), name)
	}
	var out apiVariableSetJSON
	if err == nil {
		out, err = s.toAPIVariableSet(r, sv.VariableSet)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, status, out)
}

func (s *Server) apiDeleteVariableSet(w http.ResponseWriter, r *http.Request) {
	if err := s.App.DeleteVariableSet(r.Context(), actor(r), r.PathValue("set")); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiAttachSet applies the set to one project's workspaces or one workspace ({target}: its id), or stops: 204, also
// when it already did (or didn't).
func (s *Server) apiAttachSet(project, attach bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := apiPathID(w, r, "target")
		if !ok {
			return
		}
		var name string
		if project {
			p, err := s.App.Store.ProjectByID(r.Context(), id)
			if err != nil {
				s.v1Error(w, r, err)
				return
			}
			name = p.Name
		} else {
			ws, err := s.App.Store.GetWorkspaceByID(r.Context(), id)
			if err != nil {
				s.v1Error(w, r, err)
				return
			}
			name = ws.Name
		}
		if err := s.App.AttachSet(r.Context(), actor(r), r.PathValue("set"), name, project, attach); err != nil {
			s.v1Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// VCS connections (admins).

type apiVCSConnectionJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	BaseURL      string   `json:"base_url"`
	Scope        string   `json:"scope"` // "" none, "all" or "selected": which workspaces may link its repositories
	ProjectIDs   []string `json:"project_ids"`
	WorkspaceIDs []string `json:"workspace_ids"`
}

func (s *Server) toAPIVCSConnection(r *http.Request, c storage.VCSConnection) (apiVCSConnectionJSON, error) {
	out := apiVCSConnectionJSON{ID: c.ID.String(), Name: c.Name, Provider: c.Provider, BaseURL: c.BaseURL, Scope: c.WorkspaceScope}
	var err error
	if out.ProjectIDs, err = s.projectIDs(r, c.Projects); err == nil {
		out.WorkspaceIDs, err = s.workspaceIDs(r, c.Workspaces)
	}
	return out, err
}

func (s *Server) apiVCSConnections(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.VCSConnections(r.Context(), actor(r))
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	out := []apiVCSConnectionJSON{}
	for _, c := range list {
		if name := r.URL.Query().Get("name"); name != "" && c.Name != name {
			continue
		}
		j, err := s.toAPIVCSConnection(r, c)
		if err != nil {
			s.v1Error(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeAPIv1(w, http.StatusOK, out)
}

type apiVCSConnectionInput struct {
	Name         *string   `json:"name"`     // fixed once created
	Provider     *string   `json:"provider"` // fixed
	BaseURL      *string   `json:"base_url"` // fixed
	Token        *string   `json:"token"`    // write-only; checked against the provider (it lists repositories)
	Scope        *string   `json:"scope"`
	ProjectIDs   *[]string `json:"project_ids"`   // with scope "selected"
	WorkspaceIDs *[]string `json:"workspace_ids"` // with scope "selected"
}

// apiCreateVCSConnection creates a connection, its token checked against the provider: 201 with it.
func (s *Server) apiCreateVCSConnection(w http.ResponseWriter, r *http.Request) {
	var in apiVCSConnectionInput
	if !readJSON(w, r, &in) {
		return
	}
	id, err := s.App.CreateVCSConnection(r.Context(), actor(r), app.NewVCSConnection{Name: strings.TrimSpace(deref(in.Name)),
		Provider: deref(in.Provider), BaseURL: strings.TrimSpace(deref(in.BaseURL)), Token: deref(in.Token)})
	if err == nil && (in.Scope != nil || in.ProjectIDs != nil || in.WorkspaceIDs != nil) {
		err = s.setVCSScope(r, id, in)
	}
	s.writeVCSConnection(w, r, id, http.StatusCreated, err)
}

func (s *Server) apiGetVCSConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(w, r, "id")
	if ok {
		s.writeVCSConnection(w, r, id, http.StatusOK, nil)
	}
}

// apiUpdateVCSConnection replaces the token or changes the scope: 200. Name, provider and base URL are fixed (422).
func (s *Server) apiUpdateVCSConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(w, r, "id")
	if !ok {
		return
	}
	var in apiVCSConnectionInput
	if !readJSON(w, r, &in) {
		return
	}
	c, err := s.App.VCSConnection(r.Context(), actor(r), id)
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	fields := map[string]string{}
	for field, v := range map[string]*string{"name": in.Name, "provider": in.Provider, "base_url": in.BaseURL} {
		if v != nil && strings.TrimSpace(*v) != map[string]string{"name": c.Name, "provider": c.Provider, "base_url": c.BaseURL}[field] {
			fields[field] = "can't change: create another connection"
		}
	}
	if len(fields) > 0 {
		s.v1Error(w, r, &app.ValidationError{Fields: fields})
		return
	}
	if in.Token != nil {
		_, err = s.App.ReplaceVCSToken(r.Context(), actor(r), id, *in.Token)
	}
	if err == nil && (in.Scope != nil || in.ProjectIDs != nil || in.WorkspaceIDs != nil) {
		err = s.setVCSScope(r, id, in)
	}
	s.writeVCSConnection(w, r, id, http.StatusOK, err)
}

// setVCSScope sets the connection's scope from in, the fields not sent as they are.
func (s *Server) setVCSScope(r *http.Request, id uuid.UUID, in apiVCSConnectionInput) error {
	c, err := s.App.VCSConnection(r.Context(), actor(r), id)
	if err != nil {
		return err
	}
	scope := app.VCSScope{Scope: c.WorkspaceScope, Projects: c.Projects, Workspaces: c.Workspaces}
	if in.Scope != nil {
		scope.Scope = *in.Scope
	}
	if in.ProjectIDs != nil {
		scope.Projects = nil
		for _, id := range *in.ProjectIDs {
			pid, err := uuid.Parse(id)
			if err != nil {
				return &app.ValidationError{Fields: map[string]string{"project_ids": "not a project id: " + id}}
			}
			p, err := s.App.Store.ProjectByID(r.Context(), pid)
			if err != nil {
				return &app.ValidationError{Fields: map[string]string{"project_ids": "no project " + id}}
			}
			scope.Projects = append(scope.Projects, p.Name)
		}
	}
	if in.WorkspaceIDs != nil {
		scope.Workspaces = nil
		for _, id := range *in.WorkspaceIDs {
			wid, err := uuid.Parse(id)
			if err != nil {
				return &app.ValidationError{Fields: map[string]string{"workspace_ids": "not a workspace id: " + id}}
			}
			ws, err := s.App.Store.GetWorkspaceByID(r.Context(), wid)
			if err != nil {
				return &app.ValidationError{Fields: map[string]string{"workspace_ids": "no workspace " + id}}
			}
			scope.Workspaces = append(scope.Workspaces, ws.Name)
		}
	}
	return s.App.SetVCSScope(r.Context(), actor(r), id, scope)
}

// writeVCSConnection answers with connection id as it is now, or with err.
func (s *Server) writeVCSConnection(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int, err error) {
	var c storage.VCSConnection
	if err == nil {
		c, err = s.App.VCSConnection(r.Context(), actor(r), id)
	}
	var out apiVCSConnectionJSON
	if err == nil {
		out, err = s.toAPIVCSConnection(r, c)
	}
	if err != nil {
		s.v1Error(w, r, err)
		return
	}
	writeAPIv1(w, status, out)
}

func (s *Server) apiDeleteVCSConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(w, r, "id")
	if !ok {
		return
	}
	if err := s.App.DeleteVCSConnection(r.Context(), actor(r), id); err != nil {
		s.v1Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
