package httpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/storage"
	"github.com/rudesome/form2stack/internal/vcs"
)

// Workspaces without a template: empty, or holding only variables,
// or taking their configuration from a VCS branch. Runs need configuration (a template or a branch).

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// vcsForm is the repository part of the new-workspace and link forms.
type vcsForm struct {
	Conns []storage.VCSConnection
	Repos []string // of all connections, for the repo dropdown
	Form  map[string]string
	Error string
}

// loadVCSForm lists connections and their repos; with VCS off it's empty and the form hides the part.
func (s *Server) loadVCSForm(ctx context.Context, form map[string]string, errMsg string) (vcsForm, error) {
	f := vcsForm{Form: form, Error: errMsg}
	if !s.Features.VCS {
		return f, nil
	}
	conns, err := s.Store.VCSConnections(ctx)
	if err != nil {
		return f, err
	}
	f.Conns = conns
	for _, sc := range conns {
		c, err := vcs.ConnFor(s.SecretKey, sc)
		if err == nil {
			var repos []string
			if repos, err = c.Repos(ctx); err == nil {
				f.Repos = append(f.Repos, repos...)
			}
		}
		if err != nil { // typing the repo still works
			s.Log.Warn("list vcs repos", "connection", sc.Name, "err", err)
		}
	}
	return f, nil
}

func vcsFormValues(r *http.Request) map[string]string {
	form := map[string]string{}
	for _, k := range []string{"name", "auto_apply", "vcs_connection", "vcs_repo", "vcs_branch"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	return form
}

// vcsLink validates the form's repository link and checks the branch exists; no connection = no link.
func (s *Server) vcsLink(ctx context.Context, form map[string]string) (connID, repo, branch string, err error) {
	connID, repo, branch = form["vcs_connection"], form["vcs_repo"], form["vcs_branch"]
	if connID == "" {
		return "", "", "", nil
	}
	if !s.Features.VCS {
		return "", "", "", errors.New("VCS connections are disabled")
	}
	if branch == "" {
		branch = "main"
	}
	if !branchRe.MatchString(branch) || strings.Contains(branch, "..") {
		return "", "", "", fmt.Errorf("%q is not a branch name", branch)
	}
	c, err := s.vcsConn(ctx, connID)
	if err != nil {
		return "", "", "", err
	}
	if _, err := c.Head(ctx, repo, branch); err != nil {
		return "", "", "", fmt.Errorf("can't read branch %s of %q: %w", branch, repo, err)
	}
	return connID, repo, branch, nil
}

func (s *Server) vcsConn(ctx context.Context, connID string) (vcs.Conn, error) {
	sc, err := s.Store.GetVCSConnection(ctx, connID)
	if errors.Is(err, sql.ErrNoRows) {
		return vcs.Conn{}, errors.New("the VCS connection no longer exists")
	}
	if err != nil {
		return vcs.Conn{}, err
	}
	return vcs.ConnFor(s.SecretKey, sc)
}

func (s *Server) newWorkspacePage(w http.ResponseWriter, r *http.Request) {
	// "From a template" goes to that template's form.
	if id := r.URL.Query().Get("template"); id != "" {
		http.Redirect(w, r, "/templates/"+url.PathEscape(id), http.StatusSeeOther)
		return
	}
	s.renderNewWorkspace(w, r, http.StatusOK, map[string]string{"vcs_branch": "main"}, "")
}

func (s *Server) renderNewWorkspace(w http.ResponseWriter, r *http.Request, status int, form map[string]string, errMsg string) {
	f, err := s.loadVCSForm(r.Context(), form, errMsg)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	s.render(w, r, status, "newworkspace.html", map[string]any{"Templates": s.Templates.List(), "VCS": f})
}

// createEmptyWorkspace creates a workspace without a template, optionally linked to a branch.
// It queues no run: the next step is usually variables or a branch.
func (s *Server) createEmptyWorkspace(w http.ResponseWriter, r *http.Request) {
	form := vcsFormValues(r)
	fail := func(msg string) { s.renderNewWorkspace(w, r, http.StatusUnprocessableEntity, form, msg) }
	name := form["name"]
	if !validWorkspaceName(name) {
		fail("Workspace name: " + workspaceNameRule)
		return
	}
	connID, repo, branch, err := s.vcsLink(r.Context(), form)
	if err != nil {
		fail(err.Error())
		return
	}
	u := auth.FromContext(r.Context())
	ws, err := s.Store.CreateEmptyWorkspace(r.Context(), name, form["auto_apply"] != "", u.ID)
	if errors.Is(err, storage.ErrNameTaken) {
		fail(err.Error())
		return
	}
	if err == nil && connID != "" {
		err = s.Store.SetWorkspaceVCS(r.Context(), ws.ID, connID, repo, branch)
	}
	if err != nil {
		s.internalError(w, "create workspace", err)
		return
	}
	s.Log.Info("workspace created", "workspace", ws.Name, "repo", repo, "branch", branch, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// loadLinkable loads a workspace whose repository link may change: operate access, no template,
// and no active run (its apply must fetch from the repository its plan used).
func (s *Server) loadLinkable(w http.ResponseWriter, r *http.Request) (storage.Workspace, bool) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return ws, false
	}
	if ws.TemplateID != "" {
		http.Error(w, "this workspace gets its configuration from its template", http.StatusConflict)
		return ws, false
	}
	return ws, true
}

func (s *Server) workspaceVCSPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadLinkable(w, r)
	if !ok {
		return
	}
	branch := ws.VCSBranch
	if branch == "" {
		branch = "main"
	}
	s.renderWorkspaceVCS(w, r, http.StatusOK, ws, map[string]string{"vcs_connection": ws.VCSConnectionID,
		"vcs_repo": ws.VCSRepo, "vcs_branch": branch}, "")
}

func (s *Server) renderWorkspaceVCS(w http.ResponseWriter, r *http.Request, status int, ws storage.Workspace, form map[string]string, errMsg string) {
	f, err := s.loadVCSForm(r.Context(), form, errMsg)
	if err != nil {
		s.internalError(w, "vcs connections", err)
		return
	}
	s.render(w, r, status, "workspacevcs.html", map[string]any{"W": ws, "VCS": f, "CanOperate": true, "Page": "vcs"}) // CanOperate, Page: the settings menu
}

func (s *Server) linkWorkspaceVCS(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadLinkable(w, r)
	if !ok {
		return
	}
	form := vcsFormValues(r)
	runs, err := s.Store.Runs(r.Context(), ws.ID, 1)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	if len(runs) > 0 && runs[0].Active() {
		s.renderWorkspaceVCS(w, r, http.StatusConflict, ws, form, storage.ErrRunActive.Error())
		return
	}
	connID, repo, branch, err := s.vcsLink(r.Context(), form)
	if err != nil {
		s.renderWorkspaceVCS(w, r, http.StatusUnprocessableEntity, ws, form, err.Error())
		return
	}
	if err := s.Store.SetWorkspaceVCS(r.Context(), ws.ID, connID, repo, branch); err != nil {
		s.internalError(w, "link workspace vcs", err)
		return
	}
	s.Log.Info("workspace vcs", "workspace", ws.Name, "repo", repo, "branch", branch, "user", auth.FromContext(r.Context()).Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}

// pinCommit records the head of the workspace's branch as the commit a plan job runs.
func (s *Server) pinCommit(ctx context.Context, j storage.Job) error {
	ws, err := s.Store.GetWorkspaceByID(ctx, j.WorkspaceID)
	if err != nil {
		return err
	}
	c, err := s.vcsConn(ctx, ws.VCSConnectionID)
	if err != nil {
		return err
	}
	head, err := c.Head(ctx, ws.VCSRepo, ws.VCSBranch)
	if err != nil {
		return fmt.Errorf("vcs: %w", err)
	}
	_, err = s.Store.PinVCSCommit(ctx, j.RunID, head)
	return err
}

// vcsArchivePath ends in .tar.gz so Terraform's getter unpacks it; under /v1/, it's public and
// authorised by its signature.
func vcsArchivePath(runID string) string { return "/v1/vcs-archives/" + runID + ".tar.gz" }

// vcsArchive serves a run's pinned commit of its workspace's repository to the runner.
func (s *Server) vcsArchive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	runID, ok := strings.CutSuffix(r.PathValue("file"), ".tar.gz")
	if !ok || !s.Registry.VerifyArchiveURL(r.URL.Path, q.Get("expires"), q.Get("sig")) {
		http.Error(w, "invalid or expired download link", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	run, _, err := s.Store.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && run.VCSCommit == "") {
		http.NotFound(w, r)
		return
	}
	var ws storage.Workspace
	if err == nil {
		ws, err = s.Store.GetWorkspaceByID(ctx, run.WorkspaceID)
	}
	if err != nil {
		s.internalError(w, "vcs archive", err)
		return
	}
	c, err := s.vcsConn(ctx, ws.VCSConnectionID)
	var b []byte
	if err == nil {
		b, err = c.Archive(ctx, ws.VCSRepo, run.VCSCommit, "commit")
	}
	if err != nil {
		s.Log.Warn("vcs archive", "run", run.ID, "repo", ws.VCSRepo, "err", err)
		http.Error(w, "fetching the repository failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Write(b)
}

// rollbackState makes a copy of an older state version the workspace's current state:
// same content, serial one above the current one, older versions kept.
// It holds the state lock and refuses during a run, so no run writes or applies a plan meanwhile.
func (s *Server) rollbackState(w http.ResponseWriter, r *http.Request) {
	ws, _, ok := s.loadWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	ctx := r.Context()
	from, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	u := auth.FromContext(ctx)
	lockID := rand.Text()
	info, _ := json.Marshal(map[string]string{"ID": lockID, "Operation": "OperationTypeRollback", "Who": u.Email})
	if _, ok, err := s.Store.LockState(ctx, ws.ID, lockID, info); err != nil {
		s.internalError(w, "lock state", err)
		return
	} else if !ok {
		http.Error(w, "the state is locked", http.StatusConflict)
		return
	}
	defer func() {
		if err := s.Store.UnlockState(context.WithoutCancel(ctx), ws.ID, lockID); err != nil {
			s.Log.Error("unlock state", "workspace", ws.Name, "err", err)
		}
	}()
	// After locking: a run that starts now can't touch the state until the rollback is done, and one
	// that is between phases (e.g. waiting for confirmation) would apply a plan made against the old state.
	runs, err := s.Store.Runs(ctx, ws.ID, 1)
	if err != nil {
		s.internalError(w, "list runs", err)
		return
	}
	if len(runs) > 0 && runs[0].Active() {
		http.Error(w, storage.ErrRunActive.Error(), http.StatusConflict)
		return
	}
	key, err := s.Store.StateVersionBlob(ctx, ws.ID, from)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, "state version", err)
		return
	}
	cur, err := s.Store.StateVersions(ctx, ws.ID, 1)
	if err != nil {
		s.internalError(w, "list state versions", err)
		return
	}
	if cur[0].ID == from {
		http.Error(w, "this is already the current state version", http.StatusConflict)
		return
	}
	old, err := s.StateStore.ReadAll(ctx, key)
	if err != nil {
		s.internalError(w, "read state", err)
		return
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(old, &state); err != nil {
		s.internalError(w, "parse state", err)
		return
	}
	state["serial"] = json.RawMessage(strconv.FormatInt(cur[0].Serial+1, 10))
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		s.internalError(w, "encode state", err)
		return
	}
	if code, msg := s.saveState(ctx, ws.ID, body); code != http.StatusOK {
		http.Error(w, msg, code) // e.g. a version of another lineage
		return
	}
	if err := s.Store.CopyStateIndex(ctx, ws.ID, from); err != nil {
		s.Log.Error("copy state index", "workspace", ws.Name, "err", err) // the next apply indexes it
	}
	s.Log.Info("state rolled back", "workspace", ws.Name, "version", from, "user", u.Email)
	http.Redirect(w, r, "/workspaces/"+ws.Name, http.StatusSeeOther)
}
