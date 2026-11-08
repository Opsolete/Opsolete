package httpserver

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/storage"
)

// Terraform's cloud block and `terraform login`: https://developer.hashicorp.com/terraform/cli/cloud/settings
// Opsolete serves the part of Terraform's remote API (tfe.v2) that Terraform uses with local
// execution: the organization (one, ORGANIZATION), workspaces by name, state versions and locks.
// Entitlements say "no remote operations", so plan and apply always run on the user's machine and
// Opsolete is the state backend, with the same versions, locks and outputs as its runs.
const cloudPrefix = "/api/v2"

const (
	oauthClient = "terraform-cli"
	codeTTL     = 5 * time.Minute
	// users list and revoke their own tokens under Account settings › Tokens
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
		w.Header().Set("TFP-AppName", "Opsolete")
		w.WriteHeader(http.StatusNoContent)
	})
	p := cloudPrefix
	mux.HandleFunc("GET "+p+"/organizations/{org}/entitlement-set", s.cloudUser(func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, apiObject{"entitlement-sets", s.Organization,
			map[string]any{"operations": false, "state-storage": true}, nil})
	}))
	mux.HandleFunc("GET "+p+"/organizations/{org}/workspaces/{name}", s.cloudUser(s.cloudGetWorkspace))
	mux.HandleFunc("POST "+p+"/organizations/{org}/workspaces", s.cloudUser(s.cloudCreateWorkspace))
	// cloud { workspaces { project = "x" } }: Terraform looks the project up by name, creates it if
	// it's missing and the workspace is too, then creates the workspace in it.
	mux.HandleFunc("GET "+p+"/organizations/{org}/projects", s.cloudUser(s.cloudProjects))
	mux.HandleFunc("POST "+p+"/organizations/{org}/projects", s.cloudUser(s.cloudCreateProject))
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
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Attributes    map[string]any `json:"attributes"`
	Relationships map[string]any `json:"relationships,omitempty"`
}

// apiRef is a JSON:API relationship to one object.
func apiRef(typ, id string) map[string]any {
	return map[string]any{"data": map[string]string{"type": typ, "id": id}}
}

func writeAPI(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// writeAPIError uses JSON:API error objects: go-tfe before v1.104 (Terraform < 1.15.3) reads only
// their titles, and needs them to recognise e.g. "is locked by Run" or a missing upload URL.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"status": strconv.Itoa(status), "title": msg}}})
}

func (s *Server) cloudError(w http.ResponseWriter, what string, err error) {
	s.App.Log.Error(what, "err", err)
	writeAPIError(w, http.StatusInternalServerError, "internal error")
}

// cloudUser authenticates a user token (terraform login) and puts its user on the request. Any
// organization but ours is a 404.
func (s *Server) cloudUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, email, role, groups, err := s.App.Store.TokenUser(r.Context(), token)
		if errors.Is(err, storage.ErrNotFound) {
			writeAPIError(w, http.StatusUnauthorized, "missing, invalid or expired token: run terraform login "+s.App.Registry.PublicURL.Host)
			return
		}
		if err != nil {
			s.cloudError(w, "token user", err)
			return
		}
		if org := r.PathValue("org"); org != "" && org != s.Organization {
			writeAPIError(w, http.StatusNotFound, "organization not found")
			return
		}
		// Admin only if the last login was (its email claim, which the stored email may not be) and the config still says so.
		if role == "admin" {
			role = s.Auth.Role(email, groups)
		}
		u := auth.User{ID: id, Email: email, Role: role}
		auth.SetPrincipal(r.Context(), u, "token")
		next(w, r.WithContext(auth.WithUser(r.Context(), u)))
	}
}

// cloudState opens the state of the {id} or {name} workspace if the user has at least need
// (app.StateByID, app.State), else writes the error (apiError). Everything but reading the workspace
// needs operate: the state holds secrets in plain text.
func (s *Server) cloudState(w http.ResponseWriter, r *http.Request, need app.Level) (app.State, bool) {
	var st app.State
	var err error
	if r.PathValue("id") != "" {
		id, _ := pathID(r, "id") // not an id: not found
		st, err = s.App.StateByID(r.Context(), actor(r), id, need)
	} else {
		st, err = s.App.State(r.Context(), actor(r), r.PathValue("name"), need)
	}
	if err != nil {
		s.apiError(w, r, err)
		return st, false
	}
	return st, true
}

func writeWorkspace(w http.ResponseWriter, status int, ws storage.Workspace, locked bool) {
	// Terraform runs locally whatever the mode (entitlements: no operations); remote mode refuses it the lock.
	// "latest" tells Terraform not to compare versions; any CLI version may use the state.
	writeAPI(w, status, apiObject{"workspaces", ws.ID.String(), map[string]any{"name": ws.Name, "locked": locked,
		"execution-mode": ws.ExecutionMode, "operations": false, "terraform-version": "latest"},
		map[string]any{"project": apiRef("projects", ws.ProjectID.String())}})
}

func (s *Server) cloudGetWorkspace(w http.ResponseWriter, r *http.Request) {
	need := app.LevelRead
	if r.Method == http.MethodPatch {
		need = app.LevelOperate
	}
	st, ok := s.cloudState(w, r, need)
	if !ok {
		return
	}
	lock, err := st.CurrentLock(r.Context())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeWorkspace(w, http.StatusOK, st.Workspace, lock != nil)
}

// cloudCreateWorkspace: terraform init creates the cloud block's workspace if it doesn't exist,
// as a blank workspace of the token's user, with local execution.
func (s *Server) cloudCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data struct {
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
			Relationships struct {
				Project struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"project"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "body must be a JSON:API workspace")
		return
	}
	pid := uuid.Nil // no project in the cloud block: the default one
	if id := body.Data.Relationships.Project.Data.ID; id != "" {
		pid, _ = parseID(id)
		if pid == uuid.Nil { // not an id: no such project
			s.apiError(w, r, &app.ValidationError{Message: "no such project"})
			return
		}
	}
	// local: Terraform runs on the user's machine; a remote-mode workspace would refuse this CLI its lock
	ws, err := s.App.CreateEmptyWorkspace(r.Context(), actor(r), app.EmptyWorkspace{Name: body.Data.Attributes.Name, ProjectID: pid, ExecutionMode: "local"})
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeWorkspace(w, http.StatusCreated, ws, false) // new: not locked
}

func projectObject(p storage.Project) apiObject {
	return apiObject{Type: "projects", ID: p.ID.String(), Attributes: map[string]any{"name": p.Name, "description": p.Description}}
}

// cloudProjects lists the projects the user may see; filter[names] (comma-separated) picks by name.
func (s *Server) cloudProjects(w http.ResponseWriter, r *http.Request) {
	list, err := s.App.Projects(r.Context(), actor(r))
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	names := strings.Split(r.URL.Query().Get("filter[names]"), ",")
	out := []apiObject{} // [] not null: go-tfe decodes a list
	for _, p := range list {
		if names[0] == "" || slices.Contains(names, p.Name) {
			out = append(out, projectObject(p))
		}
	}
	writeAPI(w, http.StatusOK, out)
}

// cloudCreateProject: Terraform creates the cloud block's project if it's missing. Admins only, as in the UI.
func (s *Server) cloudCreateProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Data struct {
			Attributes struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "body must be a JSON:API project")
		return
	}
	name := body.Data.Attributes.Name
	p, err := s.App.CreateProject(r.Context(), actor(r), name, body.Data.Attributes.Description)
	if errors.Is(err, app.ErrForbidden) { // Terraform shows this when the cloud block's project is missing
		writeAPIError(w, http.StatusForbidden, fmt.Sprintf("there is no project %q you may use: an Opsolete admin creates projects", name))
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeAPI(w, http.StatusCreated, projectObject(p))
}

// cloudLock: actions/lock, unlock and force-unlock. The lock is the same state lock runs take, so
// a CLI and a run never write the state at once. Unlock releases only the user's own CLI lock,
// force-unlock anyone's CLI lock; locks of runs and rollbacks end with them.
func (s *Server) cloudLock(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cloudState(w, r, app.LevelOperate)
	if !ok {
		return
	}
	ctx := r.Context()
	switch r.PathValue("action") {
	case "lock":
		if err := st.LockCLI(ctx); err != nil {
			s.apiError(w, r, err)
			return
		}
		writeWorkspace(w, http.StatusOK, st.Workspace, true)
		return
	case "unlock", "force-unlock":
		if err := st.Unlock(ctx, r.PathValue("action") == "force-unlock"); err != nil {
			s.apiError(w, r, err)
			return
		}
		lock, err := st.CurrentLock(ctx)
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		// go-tfe turns these phrases into its "locked by" errors.
		if lock != nil && lock.Cloud() {
			writeAPIError(w, http.StatusConflict, "workspace is locked by User: another user's terraform holds it (force-unlock releases it)")
			return
		} else if lock != nil {
			writeAPIError(w, http.StatusConflict, "workspace is locked by Run: a Opsolete run or rollback holds it")
			return
		}
		writeWorkspace(w, http.StatusOK, st.Workspace, false)
	default:
		writeAPIError(w, http.StatusNotFound, "unknown action")
	}
}

func stateVersionObject(workspaceID uuid.UUID, v storage.StateVersion) apiObject {
	return apiObject{"state-versions", strconv.FormatInt(v.ID, 10), map[string]any{"serial": v.Serial, "status": "finalized",
		"hosted-state-download-url": fmt.Sprintf("%s/workspaces/%s/state-versions/%d/download", cloudPrefix, workspaceID, v.ID)}, nil}
}

func (s *Server) cloudCurrentState(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cloudState(w, r, app.LevelOperate)
	if !ok {
		return
	}
	vs, err := st.Versions(r.Context(), 1)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	if len(vs) == 0 {
		writeAPIError(w, http.StatusNotFound, "the workspace has no state yet")
		return
	}
	writeAPI(w, http.StatusOK, stateVersionObject(st.Workspace.ID, vs[0]))
}

// cloudOutputs lists the current outputs by name only. Without a detailed-type Terraform falls back
// to reading the whole state for their values, so this needs no copy of them.
func (s *Server) cloudOutputs(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cloudState(w, r, app.LevelOperate)
	if !ok {
		return
	}
	outputs, _, err := st.Outputs(r.Context())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	list := []apiObject{}
	for name := range outputs {
		list = append(list, apiObject{"state-version-outputs", "wsout-" + name, map[string]any{"name": name}, nil})
	}
	writeAPI(w, http.StatusOK, list)
}

func (s *Server) cloudDownloadState(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cloudState(w, r, app.LevelOperate)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "state version not found")
		return
	}
	b, err := st.File(r.Context(), id)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// cloudCreateState saves a state version. It must come with the state in the body: go-tfe first
// tries without (the newer upload-URL flow) and, on exactly this error, retries with it.
func (s *Server) cloudCreateState(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cloudState(w, r, app.LevelOperate)
	if !ok {
		return
	}
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 3*app.MaxStateSize)).Decode(&body); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("state larger than %d MB", app.MaxStateSize>>20))
		} else {
			writeAPIError(w, http.StatusBadRequest, "not a JSON:API state version")
		}
		return
	}
	a := body.Data.Attributes
	if a.State == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "param is missing or the value is empty: state")
		return
	}
	state, err := base64.StdEncoding.DecodeString(a.State)
	sum := md5.Sum(state)
	if err != nil || a.MD5 != "" && a.MD5 != hex.EncodeToString(sum[:]) {
		writeAPIError(w, http.StatusUnprocessableEntity, "state is not base64, or doesn't match its md5")
		return
	}
	if len(state) > app.MaxStateSize { // the body limit leaves room for json-state; the state itself has the http backend's limit
		writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("state larger than %d MB", app.MaxStateSize>>20))
		return
	}
	jsonState, _ := base64.StdEncoding.DecodeString(a.JSONState) // unreadable: the version stays unindexed
	v, err := st.SaveCLIState(r.Context(), state, jsonState)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeAPI(w, http.StatusCreated, stateVersionObject(st.Workspace.ID, v))
}

// authzRequest is Terraform's OAuth authorization request (authorization code + PKCE S256).
type authzRequest struct {
	User        uuid.UUID `json:"u"`
	RedirectURI string    `json:"r"`
	Challenge   string    `json:"c"`
	Expires     int64     `json:"e"`
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
	m := hmac.New(sha256.New, s.App.Registry.Key)
	m.Write([]byte("opsolete oauth code\x00" + code))
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
	token, err := s.App.Store.CreateUserToken(r.Context(), req.User, "terraform login", userTokenTTL)
	if err != nil {
		s.App.Log.Error("create user token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.App.Log.Info("user token created", "via", "terraform login", "user", req.User)
	writeJSON(w, map[string]any{"access_token": token, "token_type": "bearer", "expires_in": int(userTokenTTL.Seconds())})
}
