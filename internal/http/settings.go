package httpserver

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// Workspace settings: the Settings tab of workspace.html (General, Health, Locking, Team access,
// Destruction and deletion) and its forms. Version control is workspaceconfig.go's.

// settingsTab is the part of the Settings tab's sub-page.
type settingsTab struct {
	Grants, ProjectGrants []storage.Grant   // Team access
	Groups                []string          // Team access: known groups, to grant
	StateConsumers        picker            // General: workspaces to share the state with
	Projects              []storage.Project // General: where an admin can move it
	StateAddress          string            // General: the state's address for terraform_remote_state
}

// loadSettings loads what the settings sub-page sub of st's workspace shows; it writes the error and
// returns ok=false otherwise.
func (s *Server) loadSettings(w http.ResponseWriter, r *http.Request, st app.State, sub string) (*settingsTab, bool) {
	ctx, ws := r.Context(), st.Workspace
	t := &settingsTab{StateAddress: s.App.Registry.PublicURL.JoinPath(runnerapi.StatePrefix, ws.ID.String()).String(),
		StateConsumers: picker{Name: "remote_state_consumers", Label: "Workspaces to share the state with"}}
	if sub == "access" {
		acc, err := s.App.Access(ctx, actor(r), ws.Name)
		if err != nil {
			s.uiError(w, r, err)
			return nil, false
		}
		t.Grants, t.ProjectGrants, t.Groups = acc.Grants, acc.ProjectGrants, acc.Groups
	}
	if sub != "" {
		return t, true
	}
	// General: workspaces to share the state with, the ones it already does first: they stay
	// selected even if this user can't see them
	cur, err := st.RemoteStateConsumers(ctx)
	if err != nil {
		s.uiError(w, r, err)
		return nil, false
	}
	all, err := s.App.ListWorkspaces(ctx, actor(r), "")
	if err != nil {
		s.internalError(w, "list workspaces", err)
		return nil, false
	}
	seen := map[uuid.UUID]bool{ws.ID: true}
	for _, c := range cur {
		seen[c.ID] = true
		t.StateConsumers.Items = append(t.StateConsumers.Items, pickerItem{c.ID.String(), c.Name, true})
		t.StateConsumers.Selected++
	}
	for _, o := range all {
		if !seen[o.ID] {
			t.StateConsumers.Items = append(t.StateConsumers.Items, pickerItem{o.ID.String(), o.Name, false})
		}
	}
	if actor(r).Admin {
		if t.Projects, err = s.App.Projects(ctx, actor(r)); err != nil {
			s.uiError(w, r, err)
			return nil, false
		}
	}
	return t, true
}

// saveGeneral saves the General page's name, execution mode and auto-apply (app.SaveGeneralSettings).
func (s *Server) saveGeneral(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ws, err := s.App.SaveGeneralSettings(r.Context(), actor(r), r.PathValue("name"), app.GeneralSettings{Name: f.Get("name"),
		ExecutionMode: f.Get("execution_mode"), AutoApply: f.Get("auto_apply") != ""})
	if s.failed(w, r, err, "general", f, s.onWorkspacePage(w, r, "settings", "")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+ws.Name+"/settings", http.StatusSeeOther)
}

// shareState saves the General page's remote state sharing: remote_state=all shares with all
// workspaces, else with the remote_state_consumers (ids) (app.ShareState).
func (s *Server) shareState(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	var ids []uuid.UUID
	for _, v := range f["remote_state_consumers"] {
		if id, ok := parseID(v); ok { // not an id: ignored, as unknown ids are
			ids = append(ids, id)
		}
	}
	if err := s.App.ShareState(r.Context(), actor(r), r.PathValue("name"), f.Get("remote_state") == "all", ids); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings", http.StatusSeeOther)
}

// saveHealth saves the Health page: drift detection (app.SaveHealthSettings).
func (s *Server) saveHealth(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	if err := s.App.SaveHealthSettings(r.Context(), actor(r), r.PathValue("name"), f.Get("drift_detection") != ""); err != nil {
		s.uiError(w, r, err)
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings/health", http.StatusSeeOther)
}

// lockWorkspace locks (action=lock, with an optional reason), unlocks or force-unlocks a workspace,
// from its Locking page (app.State.Lock, Unlock).
func (s *Server) lockWorkspace(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	ctx, action := r.Context(), f.Get("action")
	if action != "lock" && action != "unlock" && action != "force-unlock" { // the page's buttons: not user input
		http.Error(w, "action is lock, unlock or force-unlock", http.StatusBadRequest)
		return
	}
	st, err := s.App.State(ctx, actor(r), r.PathValue("name"), app.LevelOperate)
	if err == nil && action == "lock" {
		err = st.Lock(ctx, strings.TrimSpace(f.Get("reason")))
	} else if err == nil {
		err = st.Unlock(ctx, action == "force-unlock")
	}
	if s.failed(w, r, err, "lock", f, s.onWorkspacePage(w, r, "settings", "lock")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+st.Workspace.Name+"/settings/lock", http.StatusSeeOther)
}

// workspaceAccess grants an IdP group read or operate access to a workspace (level "" revokes). Admins only.
func (s *Server) workspaceAccess(w http.ResponseWriter, r *http.Request) {
	f, ok := readForm(w, r)
	if !ok {
		return
	}
	err := s.App.SetGrant(r.Context(), actor(r), r.PathValue("name"), f.Get("group"), f.Get("level"))
	if s.failed(w, r, err, "access", f, s.onWorkspacePage(w, r, "settings", "access")) {
		return
	}
	http.Redirect(w, r, "/workspaces/"+r.PathValue("name")+"/settings/access", http.StatusSeeOther)
}
