package httpserver

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/rudesome/form2stack/internal/auth"
	"github.com/rudesome/form2stack/internal/runnerapi"
	"github.com/rudesome/form2stack/internal/storage"
)

// Terraform's cloud block and `terraform login`: https://developer.hashicorp.com/terraform/cli/cloud/settings
// Form2Stack serves the part of the HCP Terraform API (tfe.v2) that Terraform uses with local
// execution: the organization (one, ORGANIZATION), workspaces by name, state versions and locks.
// Entitlements say "no remote operations", so plan and apply always run on the user's machine and
// Form2Stack is the state backend, with the same versions, locks and outputs as its runs.
const cloudPrefix = "/api/v2"

const (
	oauthClient = "terraform-cli"
	codeTTL     = 5 * time.Minute
	// shortcut: no token list or revoke page; tokens just expire. Add one if users must revoke them early.
	userTokenTTL = 30 * 24 * time.Hour
)

// discovery is Terraform's service discovery: https://developer.hashicorp.com/terraform/internals/remote-service-discovery
func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	d := map[string]any{}
	if s.Features.Registry {
		d["modules.v1"] = "/v1/modules/"
	}
	if s.Features.Deployments {
		d["tfe.v2"] = cloudPrefix + "/"
		d["login.v1"] = map[string]any{"client": oauthClient, "grant_types": []string{"authz_code"},
			"authz": "/oauth/authorization", "token": "/oauth/token", "ports": []int{10000, 10010}}
	}
	writeJSON(w, d)
}

func (s *Server) cloudRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorization", s.authorizePage)
	mux.HandleFunc("POST /oauth/authorization", s.authorize)
	mux.HandleFunc("POST /oauth/token", s.oauthToken)
	// go-tfe reads the API version from ping's headers, whatever the status.
	mux.HandleFunc("GET "+cloudPrefix+"/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("TFP-API-Version", "2.5")
		w.Header().Set("TFP-AppName", "Form2Stack")
		w.WriteHeader(http.StatusNoContent)
	})
	p := cloudPrefix
	mux.HandleFunc("GET "+p+"/organizations/{org}/entitlement-set", s.cloudUser(func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, apiObject{"entitlement-sets", s.Organization,
			map[string]any{"operations": false, "state-storage": true}})
	}))
	mux.HandleFunc("GET "+p+"/organizations/{org}/workspaces/{name}", s.cloudUser(s.cloudGetWorkspace))
	mux.HandleFunc("POST "+p+"/organizations/{org}/workspaces", s.cloudUser(s.cloudCreateWorkspace))
	// Terraform sets the new workspace's terraform-version; with local execution that's the CLI's business.
	mux.HandleFunc("PATCH "+p+"/workspaces/{id}", s.cloudUser(s.cloudGetWorkspace))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/actions/{action}", s.cloudUser(s.cloudLock))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/current-state-version", s.cloudUser(s.cloudCurrentState))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/current-state-version-outputs", s.cloudUser(s.cloudOutputs))
	mux.HandleFunc("GET "+p+"/workspaces/{id}/state-versions/{version}/download", s.cloudUser(s.cloudDownloadState))
	mux.HandleFunc("POST "+p+"/workspaces/{id}/state-versions", s.cloudUser(s.cloudCreateState))
}

// apiObject is a JSON:API resource object, the shape go-tfe decodes.
type apiObject struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Attributes map[string]any `json:"attributes"`
}

func writeAPI(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (s *Server) cloudError(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	writeErrors(w, http.StatusInternalServerError, "internal error")
}

// cloudUser authenticates a user token (terraform login) and puts its user on the request. Any
// organization but ours is a 404.
func (s *Server) cloudUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, email, role, err := s.Store.TokenUser(r.Context(), token)
		if errors.Is(err, sql.ErrNoRows) {
			writeErrors(w, http.StatusUnauthorized, "missing, invalid or expired token: run terraform login "+s.Registry.PublicURL.Host)
			return
		}
		if err != nil {
			s.cloudError(w, "token user", err)
			return
		}
		if org := r.PathValue("org"); org != "" && org != s.Organization {
			writeErrors(w, http.StatusNotFound, "organization not found")
			return
		}
		next(w, r.WithContext(auth.WithUser(r.Context(), auth.User{ID: id, Email: email, Role: role})))
	}
}

// cloudWorkspace loads the {id} or {name} workspace if the user has at least the given access;
// no access looks like no workspace (404). Everything but reading the workspace needs operate:
// the state holds secrets in plain text.
func (s *Server) cloudWorkspace(w http.ResponseWriter, r *http.Request, need string) (storage.Workspace, bool) {
	var ws storage.Workspace
	var err error
	if id := r.PathValue("id"); id != "" {
		ws, err = s.Store.GetWorkspaceByID(r.Context(), id)
	} else {
		ws, err = s.Store.GetWorkspace(r.Context(), r.PathValue("name"))
	}
	var level string
	if err == nil {
		level, err = s.accessLevel(r, ws)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows) || err == nil && level == "":
		writeErrors(w, http.StatusNotFound, "workspace not found")
	case err != nil:
		s.cloudError(w, "get workspace", err)
	case need == levelOperate && level != levelOperate:
		writeErrors(w, http.StatusForbidden, "you have read-only access to this workspace")
	default:
		return ws, true
	}
	return ws, false
}

func (s *Server) writeWorkspace(w http.ResponseWriter, r *http.Request, status int, ws storage.Workspace) {
	lock, err := s.Store.StateLockID(r.Context(), ws.ID)
	if err != nil {
		s.cloudError(w, "state lock", err)
		return
	}
	// Terraform runs locally whatever the mode (entitlements: no operations); remote mode refuses it the lock.
	// "latest" tells Terraform not to compare versions; any CLI version may use the state.
	writeAPI(w, status, apiObject{"workspaces", ws.ID, map[string]any{"name": ws.Name, "locked": lock != "",
		"execution-mode": ws.ExecutionMode, "operations": false, "terraform-version": "latest"}})
}

func (s *Server) cloudGetWorkspace(w http.ResponseWriter, r *http.Request) {
	need := levelRead
	if r.Method == http.MethodPatch {
		need = levelOperate
	}
	if ws, ok := s.cloudWorkspace(w, r, need); ok {
		s.writeWorkspace(w, r, http.StatusOK, ws)
	}
}

// cloudCreateWorkspace: terraform init creates the cloud block's workspace if it doesn't exist,
// as a blank workspace of the token's user, with local execution.
func (s *Server) cloudCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data struct {
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErrors(w, http.StatusBadRequest, "body must be a JSON:API workspace")
		return
	}
	name := body.Data.Attributes.Name
	if !validWorkspaceName(name) {
		writeErrors(w, http.StatusUnprocessableEntity, "workspace name: "+workspaceNameRule)
		return
	}
	u := auth.FromContext(r.Context())
	ws, err := s.Store.CreateEmptyWorkspace(r.Context(), name, false, u.ID)
	if errors.Is(err, storage.ErrNameTaken) {
		writeErrors(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		s.cloudError(w, "create workspace", err)
		return
	}
	if err := s.Store.SetExecutionMode(r.Context(), ws.ID, "local"); err != nil {
		s.cloudError(w, "execution mode", err)
		return
	}
	ws.ExecutionMode = "local"
	s.Log.Info("workspace created", "workspace", ws.Name, "via", "terraform cloud block", "user", u.Email)
	s.writeWorkspace(w, r, http.StatusCreated, ws)
}

// cloudLock: actions/lock, unlock and force-unlock. The lock is the same state lock runs take, so
// a CLI and a run never write the state at once. Unlock releases only the user's own CLI lock,
// force-unlock anyone's CLI lock; locks of runs and rollbacks end with them.
func (s *Server) cloudLock(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cloudWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	ctx, u := r.Context(), auth.FromContext(r.Context())
	switch r.PathValue("action") {
	case "lock":
		if ws.ExecutionMode != "local" {
			writeErrors(w, http.StatusConflict, "workspace "+ws.Name+" uses remote execution: Form2Stack runners plan and apply it. "+
				"Set its execution mode to local in the workspace's settings to run Terraform on your machine")
			return
		}
		lockID := storage.CloudLockPrefix + rand.Text()
		info, _ := json.Marshal(map[string]string{"ID": lockID, "Operation": "terraform (cloud block)", "Who": u.Email,
			"UserID": u.ID, "Created": time.Now().UTC().Format(time.RFC3339)})
		_, ok, err := s.Store.LockState(ctx, ws.ID, lockID, info)
		if err != nil {
			s.cloudError(w, "lock state", err)
			return
		}
		if !ok {
			writeErrors(w, http.StatusConflict, "workspace already locked")
			return
		}
	case "unlock", "force-unlock":
		holder := u.ID
		if r.PathValue("action") == "force-unlock" {
			holder = ""
		}
		if err := s.Store.UnlockCloudState(ctx, ws.ID, holder); err != nil {
			s.cloudError(w, "unlock state", err)
			return
		}
		lock, err := s.Store.StateLockID(ctx, ws.ID)
		if err != nil {
			s.cloudError(w, "state lock", err)
			return
		}
		// go-tfe turns these phrases into its "locked by" errors.
		if strings.HasPrefix(lock, storage.CloudLockPrefix) {
			writeErrors(w, http.StatusConflict, "workspace is locked by User: another user's terraform holds it (force-unlock releases it)")
			return
		} else if lock != "" {
			writeErrors(w, http.StatusConflict, "workspace is locked by Run: a Form2Stack run or rollback holds it")
			return
		}
	default:
		writeErrors(w, http.StatusNotFound, "unknown action")
		return
	}
	s.writeWorkspace(w, r, http.StatusOK, ws)
}

func stateVersionObject(workspaceID string, v storage.StateVersion) apiObject {
	return apiObject{"state-versions", strconv.FormatInt(v.ID, 10), map[string]any{"serial": v.Serial, "status": "finalized",
		"hosted-state-download-url": fmt.Sprintf("%s/workspaces/%s/state-versions/%d/download", cloudPrefix, workspaceID, v.ID)}}
}

func (s *Server) cloudCurrentState(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cloudWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	vs, err := s.Store.StateVersions(r.Context(), ws.ID, 1)
	if err != nil {
		s.cloudError(w, "list state versions", err)
		return
	}
	if len(vs) == 0 {
		writeErrors(w, http.StatusNotFound, "the workspace has no state yet")
		return
	}
	writeAPI(w, http.StatusOK, stateVersionObject(ws.ID, vs[0]))
}

// cloudOutputs lists the current outputs by name only. Without a detailed-type Terraform falls back
// to reading the whole state for their values, so this needs no copy of them.
func (s *Server) cloudOutputs(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cloudWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	outputs, _, err := s.Store.CurrentOutputs(r.Context(), ws.ID)
	if err != nil {
		s.cloudError(w, "current outputs", err)
		return
	}
	list := []apiObject{}
	for name := range outputs {
		list = append(list, apiObject{"state-version-outputs", "wsout-" + name, map[string]any{"name": name}})
	}
	writeAPI(w, http.StatusOK, list)
}

func (s *Server) cloudDownloadState(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cloudWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		writeErrors(w, http.StatusNotFound, "state version not found")
		return
	}
	key, err := s.Store.StateVersionBlob(r.Context(), ws.ID, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErrors(w, http.StatusNotFound, "state version not found")
		return
	}
	if err != nil {
		s.cloudError(w, "state version", err)
		return
	}
	b, err := s.readBlob(r.Context(), key)
	if err != nil {
		s.cloudError(w, "read state", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// cloudCreateState saves a state version. It must come with the state in the body: go-tfe first
// tries without (the newer upload-URL flow) and, on exactly this error, retries with it.
func (s *Server) cloudCreateState(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.cloudWorkspace(w, r, levelOperate)
	if !ok {
		return
	}
	ctx, u := r.Context(), auth.FromContext(r.Context())
	var body struct {
		Data struct {
			Attributes struct {
				MD5       string `json:"md5"`
				State     string `json:"state"`
				JSONState string `json:"json-state"`
			} `json:"attributes"`
		} `json:"data"`
	}
	// base64 state plus its JSON form
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 3*maxStateSize)).Decode(&body); err != nil {
		writeErrors(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("not a JSON:API state version, or state larger than %d MB", maxStateSize>>20))
		return
	}
	a := body.Data.Attributes
	if a.State == "" {
		writeErrors(w, http.StatusUnprocessableEntity, "param is missing or the value is empty: state")
		return
	}
	state, err := base64.StdEncoding.DecodeString(a.State)
	sum := md5.Sum(state)
	if err != nil || a.MD5 != "" && a.MD5 != hex.EncodeToString(sum[:]) {
		writeErrors(w, http.StatusUnprocessableEntity, "state is not base64, or doesn't match its md5")
		return
	}
	if locked, err := s.Store.CloudLocked(ctx, ws.ID, u.ID); err != nil {
		s.cloudError(w, "state lock", err)
		return
	} else if !locked {
		writeErrors(w, http.StatusConflict, "the workspace must be locked by you to save state")
		return
	}
	if code, msg := s.saveState(ctx, ws.ID, state); code != http.StatusOK {
		writeErrors(w, code, msg)
		return
	}
	// The JSON state lists the resources, so the workspace page shows them as after a run.
	var js tfjson.State
	if raw, err := base64.StdEncoding.DecodeString(a.JSONState); err == nil && len(raw) > 0 && json.Unmarshal(raw, &js) == nil {
		if err := s.Store.IndexState(ctx, ws.ID, runnerapi.Inventory(&js)); err != nil {
			s.Log.Error("index state", "workspace", ws.Name, "err", err) // stays unindexed: deletion keeps refusing
		}
	}
	vs, err := s.Store.StateVersions(ctx, ws.ID, 1)
	if err != nil {
		s.cloudError(w, "list state versions", err)
		return
	}
	writeAPI(w, http.StatusCreated, stateVersionObject(ws.ID, vs[0]))
}

// authzRequest is Terraform's OAuth authorization request (authorization code + PKCE S256).
type authzRequest struct {
	User        string `json:"u"`
	RedirectURI string `json:"r"`
	Challenge   string `json:"c"`
	Expires     int64  `json:"e"`
}

// parseAuthz checks an authorization request; the code may only go to Terraform's local callback.
func parseAuthz(q url.Values) (authzRequest, error) {
	ru, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		ru = &url.URL{}
	}
	port, _ := strconv.Atoi(ru.Port())
	switch {
	case q.Get("client_id") != oauthClient || q.Get("response_type") != "code":
		return authzRequest{}, errors.New("only Terraform CLI (terraform login) can request a token here")
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		return authzRequest{}, errors.New("PKCE with S256 is required")
	case ru.Scheme != "http" || ru.Hostname() != "localhost" && ru.Hostname() != "127.0.0.1" ||
		port < 10000 || port > 10010 || ru.Path != "/login" || ru.RawQuery != "" || ru.User != nil:
		return authzRequest{}, errors.New("redirect_uri must be Terraform's local login callback")
	}
	return authzRequest{RedirectURI: ru.String(), Challenge: q.Get("code_challenge")}, nil
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	if _, err := parseAuthz(r.URL.Query()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.render(w, r, http.StatusOK, "authorize.html", map[string]any{"Email": auth.FromContext(r.Context()).Email,
		"Days": int(userTokenTTL.Hours() / 24)})
}

// authorize: the user confirmed (a same-origin POST, so another site can't do it for them); send
// Terraform a short-lived code bound to the user and to its PKCE challenge.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req, err := parseAuthz(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.User, req.Expires = auth.FromContext(r.Context()).ID, time.Now().Add(codeTTL).Unix()
	payload, _ := json.Marshal(req)
	code := base64.RawURLEncoding.EncodeToString(payload)
	code += "." + s.codeMAC(code)
	http.Redirect(w, r, req.RedirectURI+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusSeeOther)
}

// codeMAC signs an authorization code. shortcut: codes are stateless, so one can be exchanged
// more than once within codeTTL, but only with its PKCE verifier, which only that Terraform has.
func (s *Server) codeMAC(code string) string {
	m := hmac.New(sha256.New, s.Registry.Key)
	m.Write([]byte("form2stack oauth code\x00" + code))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// oauthToken exchanges an authorization code and its PKCE verifier for a user token.
func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		fail("unsupported_grant_type")
		return
	}
	payload, mac, _ := strings.Cut(r.PostForm.Get("code"), ".")
	var req authzRequest
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	verifier := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !hmac.Equal([]byte(mac), []byte(s.codeMAC(payload))) || err != nil || json.Unmarshal(raw, &req) != nil ||
		time.Now().Unix() > req.Expires || r.PostForm.Get("redirect_uri") != req.RedirectURI ||
		base64.RawURLEncoding.EncodeToString(verifier[:]) != req.Challenge {
		fail("invalid_grant")
		return
	}
	token, err := s.Store.CreateUserToken(r.Context(), req.User, "terraform login", userTokenTTL)
	if err != nil {
		s.Log.Error("create user token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.Log.Info("user token created", "via", "terraform login", "user", req.User)
	writeJSON(w, map[string]any{"access_token": token, "token_type": "bearer", "expires_in": int(userTokenTTL.Seconds())})
}
