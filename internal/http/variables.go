package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Variables: environment variables (cloud credentials) and Terraform variables
// for a workspace's runs, set on the workspace (operate access) or in variable sets (admins) that
// apply to all workspaces or to the ones they're attached to. Values are sealed with SECRET_KEY.
func (s *Server) variableRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /workspaces/{name}/variables", s.setWorkspaceVariable)
	mux.HandleFunc("POST /workspaces/{name}/variables/{id}/delete", s.deleteWorkspaceVariable)
	mux.HandleFunc("GET /variable-sets", s.admin(s.variableSets))
	mux.HandleFunc("POST /variable-sets", s.admin(s.createVariableSet))
	mux.HandleFunc("GET /variable-sets/{set}", s.admin(s.variableSet))
	mux.HandleFunc("POST /variable-sets/{set}", s.admin(s.updateVariableSet))
	mux.HandleFunc("POST /variable-sets/{set}/delete", s.admin(s.deleteVariableSet))
	mux.HandleFunc("POST /variable-sets/{set}/variables", s.admin(s.setSetVariable))
	mux.HandleFunc("POST /variable-sets/{set}/variables/{id}/delete", s.admin(s.deleteSetVariable))
	mux.HandleFunc("POST /variable-sets/{set}/workspaces", s.admin(s.attachVariableSet))
}

var (
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	tfKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)
)

const maxVariableValue = 64 << 10

// reservedEnv: names that would let a variable run code on the runner or reconfigure Terraform
// itself. The runner's token claims every workspace's jobs, so a workspace must not take it over.
// shortcut: a denylist of the well-known vectors, not a sandbox; isolate runners per job if
// workspace operators aren't trusted with code execution on runners.
func reservedEnv(key string) bool {
	k := strings.ToUpper(key)
	switch k {
	case "PATH", "HOME", "SHELL", "TMPDIR", "BASH_ENV", "ENV", "NODE_OPTIONS", "PERL5OPT", "RUBYOPT",
		"SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND": // SHELLOPTS=xtrace + PS4='$(…)' runs code in bash
		return true
	}
	for _, p := range []string{"LD_", "DYLD_", "GIT_", "SSH_", "OPS_", "PYTHON"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	// TF_VAR_*: use a Terraform variable instead (terraform-exec drops them from the environment).
	return strings.HasPrefix(k, "TF_")
}

// variableFromForm validates and seals a posted variable of o.
func (s *Server) variableFromForm(r *http.Request, o storage.VarOwner) (storage.Variable, error) {
	if s.SecretKey == nil {
		return storage.Variable{}, errors.New("variables need SECRET_KEY to be set on the server")
	}
	cat, key, val := r.PostFormValue("category"), strings.TrimSpace(r.PostFormValue("key")), r.PostFormValue("value")
	switch {
	case cat != "env" && cat != "terraform":
		return storage.Variable{}, errors.New("category must be env or terraform")
	case cat == "env" && (!envKeyRe.MatchString(key) || reservedEnv(key)):
		return storage.Variable{}, errors.New("an environment variable name is letters, digits and _; " +
			"PATH, HOME, LD_*, GIT_*, SSH_*, OPS_* and TF_* are reserved (use the terraform category for Terraform variables)")
	case cat == "terraform" && !tfKeyRe.MatchString(key):
		return storage.Variable{}, errors.New("a Terraform variable name is letters, digits, _ and -, starting with a letter or _")
	case len(val) > maxVariableValue:
		return storage.Variable{}, fmt.Errorf("values are limited to %d KB", maxVariableValue>>10)
	}
	v := storage.Variable{Category: cat, Key: key, Sensitive: r.PostFormValue("sensitive") != "", Owner: o}
	var err error
	v.Value, err = vcs.Seal(s.SecretKey, val, vcs.VariableAD(v))
	return v, err
}

// varsBlock is the data of the "variables" partial.
type varsBlock struct {
	Base    string // URL the add and delete forms post under
	Vars    []varView
	CanEdit bool
	NoKey   bool
}

// setView is a variable set applied to a workspace, with its variables.
type setView struct {
	storage.VariableSet
	Vars varsBlock
}

type varView struct {
	ID, Category, Key, Value string
	Sensitive                bool
}

func (s *Server) varsBlock(ctx context.Context, o storage.VarOwner, base string, canEdit bool) (varsBlock, error) {
	vars, err := s.Store.Variables(ctx, o)
	b := varsBlock{Base: base, CanEdit: canEdit, NoKey: s.SecretKey == nil}
	for _, v := range vars {
		vv := varView{ID: v.ID, Category: v.Category, Key: v.Key, Sensitive: v.Sensitive}
		if !v.Sensitive {
			if vv.Value, err = vcs.Open(s.SecretKey, v.Value, vcs.VariableAD(v)); err != nil {
				vv.Value, err = "(can't decrypt: SECRET_KEY changed? Set it again)", nil
			}
		}
		b.Vars = append(b.Vars, vv)
	}
	return b, err
}

// runVariables opens the variables of a workspace's run: environment variables for Terraform's
// environment, and Terraform variables under the workspace's inputs (which win).
func (s *Server) runVariables(ctx context.Context, workspaceID string, inputs map[string]any) (map[string]string, map[string]any, error) {
	list, err := s.Store.RunVariables(ctx, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("load variables: %w", err)
	}
	env, vars := map[string]string{}, map[string]any{}
	for _, v := range list {
		val, err := vcs.Open(s.SecretKey, v.Value, vcs.VariableAD(v))
		if err != nil {
			return nil, nil, fmt.Errorf("can't decrypt variable %s: SECRET_KEY missing or changed? Set the variable again", v.Key)
		}
		if v.Category == "env" {
			env[v.Key] = val
		} else {
			vars[v.Key] = val
		}
	}
	maps.Copy(vars, inputs)
	return env, vars, nil
}

func (s *Server) setWorkspaceVariable(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	v, err := s.variableFromForm(r, storage.VarOwner{WorkspaceID: ws.ID})
	if _, input := ws.Inputs[v.Key]; err == nil && v.Category == "terraform" && input {
		err = fmt.Errorf("%s is an input of the workspace's template: change it with Configure", v.Key)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetVariable(r.Context(), storage.VarOwner{WorkspaceID: ws.ID}, v); err != nil {
		s.internalError(w, "set variable", err)
		return
	}
	s.Log.Info("variable set", "workspace", ws.Name, "category", v.Category, "key", v.Key, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/variables", http.StatusSeeOther)
}

func (s *Server) deleteWorkspaceVariable(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	if err := s.Store.DeleteVariable(r.Context(), storage.VarOwner{WorkspaceID: ws.ID}, r.PathValue("id")); err != nil {
		s.internalError(w, "delete variable", err)
		return
	}
	s.Log.Info("variable deleted", "workspace", ws.Name, "id", r.PathValue("id"), "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/variables", http.StatusSeeOther)
}

func (s *Server) variableSets(w http.ResponseWriter, r *http.Request) {
	sets, err := s.Store.VariableSets(r.Context())
	if err != nil {
		s.internalError(w, "list variable sets", err)
		return
	}
	s.render(w, r, http.StatusOK, "variablesets.html", sets)
}

func (s *Server) createVariableSet(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if !workspaceNameRe.MatchString(name) {
		http.Error(w, "a variable set name is 2–63 lowercase letters, digits and dashes", http.StatusBadRequest)
		return
	}
	err := s.Store.CreateVariableSet(r.Context(), name, strings.TrimSpace(r.PostFormValue("description")), r.PostFormValue("global") != "")
	if errors.Is(err, storage.ErrExists) {
		http.Error(w, "a variable set with this name already exists", http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, "create variable set", err)
		return
	}
	s.Log.Info("variable set created", "set", name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets/"+name, http.StatusSeeOther)
}

func (s *Server) loadSet(w http.ResponseWriter, r *http.Request) (storage.VariableSet, bool) {
	set, err := s.Store.GetVariableSet(r.Context(), r.PathValue("set"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return set, false
	}
	if err != nil {
		s.internalError(w, "get variable set", err)
		return set, false
	}
	return set, true
}

func (s *Server) variableSet(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	vars, err := s.varsBlock(r.Context(), storage.VarOwner{SetID: set.ID}, "/variable-sets/"+set.Name, true)
	if err != nil {
		s.internalError(w, "list variables", err)
		return
	}
	// shortcut: offers the 200 most recently active workspaces; add search if there are more.
	all, err := s.Store.ListWorkspaces(r.Context(), storage.Viewer{Admin: true}) // admin-only page
	if err != nil {
		s.internalError(w, "list workspaces", err)
		return
	}
	p := workspacePicker("workspace", "Workspaces to apply the set to", set.Workspaces, all, func(storage.Workspace) bool { return true })
	s.render(w, r, http.StatusOK, "variableset.html", map[string]any{"Set": set, "Variables": vars, "Picker": p})
}

func (s *Server) updateVariableSet(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	global := r.PostFormValue("global") != ""
	if err := s.Store.UpdateVariableSet(r.Context(), set.ID, strings.TrimSpace(r.PostFormValue("description")), global); err != nil {
		s.internalError(w, "update variable set", err)
		return
	}
	s.Log.Info("variable set updated", "set", set.Name, "global", global, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets/"+set.Name, http.StatusSeeOther)
}

func (s *Server) deleteVariableSet(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteVariableSet(r.Context(), set.ID); err != nil {
		s.internalError(w, "delete variable set", err)
		return
	}
	s.Log.Info("variable set deleted", "set", set.Name, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets", http.StatusSeeOther)
}

func (s *Server) setSetVariable(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	v, err := s.variableFromForm(r, storage.VarOwner{SetID: set.ID})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.SetVariable(r.Context(), storage.VarOwner{SetID: set.ID}, v); err != nil {
		s.internalError(w, "set variable", err)
		return
	}
	s.Log.Info("variable set", "set", set.Name, "category", v.Category, "key", v.Key, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets/"+set.Name, http.StatusSeeOther)
}

func (s *Server) deleteSetVariable(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteVariable(r.Context(), storage.VarOwner{SetID: set.ID}, r.PathValue("id")); err != nil {
		s.internalError(w, "delete variable", err)
		return
	}
	s.Log.Info("variable deleted", "set", set.Name, "id", r.PathValue("id"), "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets/"+set.Name, http.StatusSeeOther)
}

// attachVariableSet applies the set to exactly the workspaces (by name) in the workspace fields.
func (s *Server) attachVariableSet(w http.ResponseWriter, r *http.Request) {
	set, ok := s.loadSet(w, r)
	if !ok {
		return
	}
	r.ParseForm()
	want := r.PostForm["workspace"]
	err := s.syncSelection(r.Context(), set.Workspaces, want, func(ws string, on bool) error {
		return s.Store.AttachVariableSet(r.Context(), set.ID, ws, on)
	})
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "choose existing workspaces", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.internalError(w, "attach variable set", err)
		return
	}
	s.Log.Info("variable set workspaces", "set", set.Name, "workspaces", want, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/variable-sets/"+set.Name, http.StatusSeeOther)
}
