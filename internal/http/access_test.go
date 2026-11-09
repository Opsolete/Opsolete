package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/auth/authtest"
	"github.com/opsolete/opsolete/internal/registry/registrytest"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Surfaces and their principals, in the order of an access row's want.
type surface int

const (
	onUI       surface = iota // anonymous, stranger (no grant), reader (read grant), operator (operate grant), admin: session login
	onAPI                     // the same five via user tokens (terraform login); anonymous sends none
	onRunner                  // no token, a registry token, the runner token (with the claimed job's lease)
	onState                   // no credentials, a forged password, the claimed run's state credential
	onRegistry                // no token, a user token, a registry token, the runner token (which the registry refuses)
	onArchive                 // archive links: unsigned, signed
	onPublic                  // anyone
)

// accessFixture is one set of the objects the matrix requests: every name has a random suffix.
type accessFixture struct {
	project, ws, wsID, module, run, cws, cwsID, version, wsVar, set, setVar, tpl, ns, conn string
}

func (f accessFixture) path(p string) string {
	return strings.NewReplacer("{proj}", f.project, "{ws}", f.ws, "{wsid}", f.wsID, "{module}", f.module, "{run}", f.run, "{cws}", f.cws, "{cwsid}", f.cwsID,
		"{sv}", f.version, "{var}", f.wsVar, "{set}", f.set, "{setvar}", f.setVar, "{tpl}", f.tpl, "{ns}", f.ns, "{conn}", f.conn).Replace(p)
}

// accessRow is one registered pattern: the request to send and the status each principal of its surface gets.
type accessRow struct {
	pattern string // as registered; the request's method is its first word
	path    string // with accessFixture placeholders
	body    string // form (ui) or JSON (others), with placeholders
	surface surface
	want    []int
	fresh   bool // mutates: a cell after one that succeeded gets new fixtures
}

// TestAccessMatrix sends every registered route as every principal and checks the status: a
// characterization of who may do what today. A route without a row fails the test, so new routes
// can't skip it. Rows run in order: the runner and state rows first, while the claimed job is the
// only one in the queue.
func TestAccessMatrix(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sfx := strings.ToLower(newID()[:8])

	tpls, err := templates.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &app.App{Store: store, Blobs: memblob.OpenBucket(nil), Secrets: testKeys, Templates: tpls, Registry: testReg,
		VCS: vcs.NewClient(nil), Log: testLog(t)}
	all := Features{Deployments: true, Registry: true, Editor: true, VCS: true}
	server := func(au *auth.Auth) *Server {
		s := New(a, au, all)
		s.RunnerToken, s.Organization, s.ClaimWait = testRunnerToken, "acc", 10*time.Millisecond
		return s
	}
	is := authtest.New(t)
	email := func(user string) string { return "acc-" + user + "-" + sfx + "@example.com" }
	oidc, err := auth.New(ctx, auth.Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		PublicURL: testReg.PublicURL, Admins: []string{email("admin")}}, []byte("k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	anonSrv := server(oidc)
	anon := anonSrv.Handler()

	// Users: a dev login each, and a terraform login token of the same user.
	readers, operators := "acc-read-"+sfx, "acc-op-"+sfx
	users := []struct {
		name   string
		admin  bool
		groups []string
	}{{"stranger", false, []string{"acc-none-" + sfx}}, {"reader", false, []string{readers}}, {"operator", false, []string{operators}}, {"admin", true, nil}}
	uiH := []http.Handler{anon}
	tokens := []string{""}
	for _, u := range users {
		uiH = append(uiH, server(devAuth(t, store, email(u.name), u.admin, u.groups...)).Handler())
		role := map[bool]string{true: "admin", false: "user"}[u.admin]
		id, err := store.UpsertUser(ctx, "", email(u.name), role, u.groups)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := store.CreateUserToken(ctx, id, "access matrix", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok)
	}
	admin, adminToken := uiH[4], tokens[4]
	registryToken, err := store.CreateToken(ctx, "acc-"+sfx, storage.ScopeRegistry)
	if err != nil {
		t.Fatal(err)
	}

	must := func(t *testing.T, rec *httptest.ResponseRecorder, want int, what string) *httptest.ResponseRecorder {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("fixture %s: %d %s", what, rec.Code, rec.Body.String())
		}
		return rec
	}
	apiCall := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/vnd.api+json")
		return do(anon, req)
	}
	n := 0
	fixture := func(t *testing.T) accessFixture {
		t.Helper()
		n++
		s := fmt.Sprintf("%s-%d", sfx, n)
		f := accessFixture{project: "acc-" + s, ws: "acc-ws-" + s, cws: "acc-cli-" + s, set: "acc-set-" + s, ns: "acc" + strings.ReplaceAll(s, "-", ""), conn: "acc-" + s}
		must(t, post(admin, "/projects", url.Values{"name": {f.project}}), http.StatusSeeOther, "project")
		for group, level := range map[string]string{readers: "read", operators: "operate"} {
			must(t, post(admin, "/projects/"+f.project+"/access", url.Values{"group": {group}, "level": {level}}), http.StatusSeeOther, "grant")
		}
		rec := must(t, post(admin, "/templates/new", url.Values{"name": {"Acc " + s}, "module_source": {"git::https://x//m"}, "module_ref": {"v1"},
			"in_name": {"location"}, "in_type": {"string"}, "in_default": {"westeurope"}}), http.StatusSeeOther, "template")
		f.tpl = strings.TrimPrefix(rec.Header().Get("Location"), "/templates/")
		rec = must(t, post(admin, "/templates/"+f.tpl, url.Values{"ops-workspace": {f.ws}, fieldProject: {f.project}, "location": {"westeurope"}}),
			http.StatusSeeOther, "workspace")
		f.run = strings.TrimPrefix(rec.Header().Get("Location"), "/workspaces/"+f.ws+"/runs/")

		p, err := store.Project(ctx, f.project)
		if err != nil {
			t.Fatal(err)
		}
		var obj struct{ Data struct{ ID string } }
		rec = must(t, apiCall("POST", cloudPrefix+"/organizations/acc/workspaces",
			fmt.Sprintf(`{"data":{"attributes":{"name":%q},"relationships":{"project":{"data":{"id":%q}}}}}`, f.cws, p.ID)), http.StatusCreated, "cli workspace")
		json.Unmarshal(rec.Body.Bytes(), &obj)
		f.cwsID = obj.Data.ID
		must(t, apiCall("POST", cloudPrefix+"/workspaces/"+f.cwsID+"/actions/lock", ""), http.StatusOK, "cli lock")
		st := base64.StdEncoding.EncodeToString([]byte(`{"version":4,"serial":1,"lineage":"acc","outputs":{},"resources":[]}`))
		rec = must(t, apiCall("POST", cloudPrefix+"/workspaces/"+f.cwsID+"/state-versions", `{"data":{"attributes":{"state":"`+st+`"}}}`),
			http.StatusCreated, "state version")
		json.Unmarshal(rec.Body.Bytes(), &obj)
		f.version = obj.Data.ID
		must(t, apiCall("POST", cloudPrefix+"/workspaces/"+f.cwsID+"/actions/unlock", ""), http.StatusOK, "cli unlock")

		ws, err := store.GetWorkspace(ctx, f.ws)
		if err != nil {
			t.Fatal(err)
		}
		f.wsID = ws.ID.String()
		added, err := store.WorkspaceTemplates(ctx, ws.ID)
		if err != nil || len(added) != 1 {
			t.Fatalf("workspace templates: %v %v", added, err)
		}
		f.module = added[0].Name
		must(t, post(admin, "/workspaces/"+f.ws+"/variables", url.Values{"category": {"terraform"}, "key": {"k"}, "value": {"v"}}), http.StatusSeeOther, "variable")
		vars, err := store.Variables(ctx, storage.VarOwner{WorkspaceID: ws.ID})
		if err != nil || len(vars) != 1 {
			t.Fatalf("variable: %v %v", vars, err)
		}
		f.wsVar = vars[0].ID.String()
		must(t, post(admin, "/variable-sets", url.Values{"name": {f.set}}), http.StatusSeeOther, "variable set")
		must(t, post(admin, "/variable-sets/"+f.set+"/variables", url.Values{"category": {"env"}, "key": {"K"}, "value": {"v"}}), http.StatusSeeOther, "set variable")
		set, err := store.GetVariableSet(ctx, f.set)
		if err != nil {
			t.Fatal(err)
		}
		if vars, err = store.Variables(ctx, storage.VarOwner{SetID: set.ID}); err != nil || len(vars) != 1 {
			t.Fatalf("set variable: %v %v", vars, err)
		}
		f.setVar = vars[0].ID.String()

		archive := registrytest.TarGz(t, map[string]string{"main.tf": `variable "x" {}`})
		must(t, upload(admin, map[string]string{"namespace": f.ns, "name": "net", "system": "azurerm", "version": "1.0.0"}, archive), http.StatusSeeOther, "module")
		conn := storage.VCSConnection{Name: f.conn, Provider: vcs.GitHub, BaseURL: "https://api.github.com"}
		if conn.Token, err = a.Secrets.Seal("tok", app.TokenAD(conn)); err != nil {
			t.Fatal(err)
		}
		id, err := store.CreateVCSConnection(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		f.conn = id.String()
		return f
	}
	shared := fixture(t)

	// The runner's job: the shared workspace's plan, the only queued run.
	body, ok, err := a.ClaimJob(ctx, "acc")
	var job runnerapi.Job
	if err != nil || !ok || json.Unmarshal(body, &job) != nil || job.RunID != shared.run {
		t.Fatalf("claim: %v %v %s", ok, err, body)
	}

	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}
	basic := func(pass string) func(*http.Request) {
		return func(r *http.Request) { r.SetBasicAuth(job.WorkspaceID, pass) }
	}
	none := func(*http.Request) {}
	type principal struct {
		name string
		h    http.Handler
		auth func(*http.Request)
	}
	principals := map[surface][]principal{
		onRunner: {{"none", anon, none}, {"registry token", anon, bearer(registryToken)},
			{"runner", anon, func(r *http.Request) { bearer(testRunnerToken)(r); r.Header.Set(runnerapi.LeaseHeader, job.LeaseID) }}},
		onState: {{"none", anon, none}, {"forged", anon, basic("forged")}, {"run", anon, basic(job.StateToken)}},
		onRegistry: {{"none", anon, none}, {"user token", anon, bearer(adminToken)}, {"registry token", anon, bearer(registryToken)},
			{"runner token", anon, bearer(testRunnerToken)}},
		onArchive: {{"unsigned", anon, none}, {"signed", anon, func(r *http.Request) {
			u, _ := url.Parse(testReg.SignedArchiveURL(r.URL.Path, time.Minute))
			r.URL.RawQuery = u.RawQuery
		}}},
		onPublic: {{"anyone", anon, none}},
	}
	for i, name := range []string{"anonymous", "stranger", "reader", "operator", "admin"} {
		principals[onUI] = append(principals[onUI], principal{name, uiH[i], none})
		auth := none
		if tokens[i] != "" {
			auth = bearer(tokens[i])
		}
		principals[onAPI] = append(principals[onAPI], principal{name, anon, auth})
	}

	rows := accessRows()

	// Coverage: one row per registered pattern (all features on), and no row without one.
	t.Run("every route has one row", func(t *testing.T) {
		registered := anonSrv.patterns
		var covered []string
		for _, r := range rows {
			if !slices.Contains(registered, r.pattern) {
				t.Errorf("row %q: no such route", r.pattern)
			}
			if slices.Contains(covered, r.pattern) {
				t.Errorf("row %q: twice", r.pattern)
			}
			covered = append(covered, r.pattern)
		}
		for _, p := range registered {
			if !slices.Contains(covered, p) {
				t.Errorf("route %q has no access row", p)
			}
		}
	})

	for _, r := range rows {
		t.Run(r.pattern, func(t *testing.T) {
			ps := principals[r.surface]
			if len(r.want) != len(ps) {
				t.Errorf("%s: %d statuses for %d principals", r.pattern, len(r.want), len(ps))
				return
			}
			method, _, _ := strings.Cut(r.pattern, " ")
			f, dirty := shared, r.fresh
			for i, p := range ps {
				if dirty {
					f, dirty = fixture(t), false
				}
				req := httptest.NewRequest(method, f.path(r.path), strings.NewReader(f.path(r.body)))
				if r.surface == onUI || r.surface == onPublic {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				} else if r.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if strings.HasSuffix(r.pattern, "/logs/stream") { // a run's stream lasts until it ends
					c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
					defer cancel()
					req = req.WithContext(c)
				}
				p.auth(req)
				rec := do(p.h, req)
				dirty = r.fresh && rec.Code < 400
				if rec.Code != r.want[i] {
					t.Errorf("%s as %s: %d, want %d: %.200s", r.pattern, p.name, rec.Code, r.want[i], rec.Body.String())
				}
			}
		})
	}
}

func accessRows() []accessRow {
	const (
		job = runnerapi.Prefix + "/jobs/{id}"
		run = runnerapi.Prefix + "/jobs/{run}"
		sp  = runnerapi.StatePrefix + "/{id}"
		cs  = runnerapi.StatePrefix + "/{wsid}"
		org = cloudPrefix + "/organizations/{org}"
		acc = cloudPrefix + "/organizations/acc"
		cw  = cloudPrefix + "/workspaces/{id}"
		cid = cloudPrefix + "/workspaces/{cwsid}"
		w   = "/workspaces/{name}"
		ws  = "/workspaces/{ws}"
		mod = "/registry/{ns}/{name}/{system}"
		m   = "/registry/{ns}/net/azurerm"
	)
	tfstate := `{"version":4,"serial":2,"lineage":"acc","resources":[]}`
	return []accessRow{
		// Runner API and state backend first: the claimed job is the only one in the queue.
		{"POST " + runnerapi.Prefix + "/jobs/claim", runnerapi.Prefix + "/jobs/claim", `{"runner":"acc"}`, onRunner, []int{401, 401, 204}, false},
		{"POST " + job + "/ack", run + "/ack", "", onRunner, []int{401, 401, 204}, false}, // before any other call for the job
		{"POST " + job + "/heartbeat", run + "/heartbeat", "", onRunner, []int{401, 401, 204}, false},
		{"POST " + job + "/logs", run + "/logs", "line\n", onRunner, []int{401, 401, 204}, false},
		{"POST " + job + "/artifacts/{name}", run + "/artifacts/" + runnerapi.ArtifactPlan, "plan", onRunner, []int{401, 401, 204}, false},
		{"GET " + job + "/artifacts/{name}", run + "/artifacts/" + runnerapi.ArtifactPlan, "", onRunner, []int{401, 401, 409}, false},
		// a wrong password is 403, as for another run's credential
		{"GET " + sp, cs, "", onState, []int{401, 403, 204}, false},
		{"LOCK " + sp, cs, `{"ID":"acc"}`, onState, []int{401, 403, 200}, false},
		{"POST " + sp, cs + "?ID=acc", tfstate, onState, []int{401, 403, 403}, false}, // a plan never writes the state
		{"UNLOCK " + sp, cs, `{"ID":"acc"}`, onState, []int{401, 403, 200}, false},
		{"POST " + job + "/finish", run + "/finish", `{"status":"succeeded"}`, onRunner, []int{401, 401, 204}, false},

		// Public: health, discovery, login, the OAuth token exchange.
		{"GET /healthz", "/healthz", "", onPublic, []int{200}, false},
		{"GET /readyz", "/readyz", "", onPublic, []int{200}, false},
		{"GET /static/{file}", assetURLs["app.css"], "", onPublic, []int{200}, false},
		{"GET /.well-known/terraform.json", "/.well-known/terraform.json", "", onPublic, []int{200}, false},
		{"GET /auth/login", "/auth/login", "", onPublic, []int{302}, false},
		{"GET /auth/callback", "/auth/callback?state=x&code=y", "", onPublic, []int{400}, false},
		{"POST /auth/logout", "/auth/logout", "", onPublic, []int{303}, false},
		{"POST /oauth/token", "/oauth/token", "grant_type=authorization_code&code=x&code_verifier=y&redirect_uri=http%3A%2F%2Flocalhost%3A10000%2Flogin", onPublic, []int{400}, false},
		{"GET " + cloudPrefix + "/ping", cloudPrefix + "/ping", "", onPublic, []int{204}, false},

		// Registry protocol and signed archive links. A terraform login token also reads the registry.
		{"GET /v1/modules/{ns}/{name}/{system}/versions", "/v1/modules/{ns}/net/azurerm/versions", "", onRegistry, []int{401, 200, 200, 401}, false},
		{"GET /v1/modules/{ns}/{name}/{system}/{version}/download", "/v1/modules/{ns}/net/azurerm/1.0.0/download", "", onRegistry, []int{401, 204, 204, 401}, false},
		{"GET /v1/archives/{ns}/{name}/{system}/{file}", "/v1/archives/{ns}/net/azurerm/1.0.0.tar.gz", "", onArchive, []int{403, 200}, false},
		{"GET /v1/vcs-archives/{file}", "/v1/vcs-archives/{run}.tar.gz", "", onArchive, []int{403, 404}, false},

		// Cloud API (terraform login tokens). Without a project a workspace goes into the default one,
		// where only admins (and groups with operate on it) create workspaces; state reads need operate (the state holds secrets). 409: past the access check, nothing to do.
		{"GET " + org + "/entitlement-set", acc + "/entitlement-set", "", onAPI, []int{401, 200, 200, 200, 200}, false},
		{"GET " + org + "/workspaces/{name}", acc + "/workspaces/{cws}", "", onAPI, []int{401, 404, 200, 200, 200}, false},
		{"POST " + org + "/workspaces", acc + "/workspaces", `{"data":{"attributes":{"name":"{cws}-new"}}}`, onAPI, []int{401, 422, 422, 422, 201}, true},
		{"GET " + org + "/projects", acc + "/projects", "", onAPI, []int{401, 200, 200, 200, 200}, false},
		{"POST " + org + "/projects", acc + "/projects", `{"data":{"attributes":{"name":"{proj}-new"}}}`, onAPI, []int{401, 403, 403, 403, 201}, true},
		{"PATCH " + cw, cid, `{"data":{"attributes":{"terraform-version":"1.14.0"}}}`, onAPI, []int{401, 404, 403, 200, 200}, false},
		{"POST " + cw + "/actions/{action}", cid + "/actions/lock", "", onAPI, []int{401, 404, 403, 200, 200}, true},
		{"GET " + cw + "/current-state-version", cid + "/current-state-version", "", onAPI, []int{401, 404, 403, 200, 200}, false},
		{"GET " + cw + "/current-state-version-outputs", cid + "/current-state-version-outputs", "", onAPI, []int{401, 404, 403, 200, 200}, false},
		{"GET " + cw + "/state-versions/{version}/download", cid + "/state-versions/{sv}/download", "", onAPI, []int{401, 404, 403, 200, 200}, false},
		{"POST " + cw + "/state-versions", cid + "/state-versions", `{"data":{"attributes":{"state":"eyJ2ZXJzaW9uIjo0fQ=="}}}`, onAPI, []int{401, 404, 403, 409, 409}, true},

		// UI: OAuth consent, account, organization, audit.
		{"GET /oauth/authorization", "/oauth/authorization?client_id=terraform-cli&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A10000%2Flogin&state=s&code_challenge=c&code_challenge_method=S256", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"POST /oauth/authorization", "/oauth/authorization?client_id=terraform-cli&response_type=code&redirect_uri=http%3A%2F%2Flocalhost%3A10000%2Flogin&state=s&code_challenge=c&code_challenge_method=S256", "", onUI, []int{401, 303, 303, 303, 303}, false},
		{"GET /account", "/account", "", onUI, []int{302, 302, 302, 302, 302}, false},
		{"GET /account/{page}", "/account/tokens", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"POST /account/tokens/{id}/revoke", "/account/tokens/00000000-0000-0000-0000-000000000000/revoke", "", onUI, []int{401, 303, 303, 303, 303}, false},
		{"POST /account/sessions/revoke", "/account/sessions/revoke", "", onUI, []int{401, 303, 303, 303, 303}, false},
		{"POST /users/{id}/sessions/revoke", "/users/00000000-0000-0000-0000-000000000000/sessions/revoke", "", onUI, []int{401, 403, 403, 403, 303}, false},
		{"GET /organization", "/organization", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /organization", "/organization", "currency=", onUI, []int{401, 403, 403, 403, 422}, false},
		{"GET /audit", "/audit", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"GET /{$}", "/", "", onUI, []int{302, 200, 200, 200, 200}, false},

		// UI: registry pages.
		{"GET /registry", "/registry", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"GET " + mod + "/{version}", m + "/1.0.0", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"GET " + mod + "/{version}/docs.md", m + "/1.0.0/docs.md", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"GET /registry/publish", "/registry/publish", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /registry", "/registry", "", onUI, []int{401, 403, 403, 403, 400}, false}, // not multipart
		{"POST /registry/publish", "/registry/publish", "", onUI, []int{401, 403, 403, 403, 404}, false},
		{"POST " + mod + "/sync", m + "/sync", "", onUI, []int{401, 403, 403, 403, 404}, false},
		{"POST " + mod + "/unlink", m + "/unlink", "", onUI, []int{401, 403, 403, 403, 404}, false},
		{"POST " + mod + "/delete", m + "/delete", "", onUI, []int{401, 403, 403, 403, 303}, true},

		// UI: VCS connections and the template catalog.
		{"GET /vcs", "/vcs", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"GET /vcs/new", "/vcs/new", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /vcs", "/vcs", "", onUI, []int{401, 403, 403, 403, 422}, false},
		{"GET /vcs/{id}/settings", "/vcs/{conn}/settings", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /vcs/{id}/settings", "/vcs/{conn}/settings", "", onUI, []int{401, 403, 403, 403, 303}, true},
		{"POST /vcs/{id}/token", "/vcs/{conn}/token", "", onUI, []int{401, 403, 403, 403, 422}, false},
		{"POST /vcs/{id}/delete", "/vcs/{conn}/delete", "", onUI, []int{401, 403, 403, 403, 303}, true},
		{"GET /catalog", "/catalog", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /catalog", "/catalog", "", onUI, []int{401, 403, 403, 403, 422}, false},
		{"POST /catalog/sync", "/catalog/sync", "", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST /catalog/delete", "/catalog/delete", "", onUI, []int{401, 403, 403, 403, 303}, false},

		// UI: templates, the builder and rollouts.
		{"GET /templates/{id}", "/templates/{tpl}", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"POST /templates/{id}", "/templates/{tpl}", "ops-workspace={ws}-new&ops-project={proj}&location=westeurope", onUI, []int{401, 422, 422, 303, 303}, true},
		{"GET /templates/new", "/templates/new", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /templates/new", "/templates/new", "name=x&preview=1", onUI, []int{401, 403, 403, 403, 422}, false},
		{"GET /templates/{id}/edit", "/templates/{tpl}/edit", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /templates/{id}/edit", "/templates/{tpl}/edit", "name=x&preview=1", onUI, []int{401, 403, 403, 403, 422}, false},
		{"GET /templates/{id}/rollout", "/templates/{tpl}/rollout?project={proj}", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /templates/{id}/rollout", "/templates/{tpl}/rollout", "project={proj}&source=git%3A%3Ahttps%3A%2F%2Fx%2F%2Fm&ref=v2", onUI, []int{401, 403, 403, 403, 409}, true},
		{"POST /templates/{id}/rollout/{action}", "/templates/{tpl}/rollout/cancel", "project={proj}", onUI, []int{401, 403, 403, 403, 404}, true},
		{"POST /templates/{id}/delete", "/templates/{tpl}/delete", "", onUI, []int{401, 403, 403, 403, 303}, true},

		// UI: projects.
		{"GET /projects", "/projects", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"POST /projects", "/projects", "name={proj}-new", onUI, []int{401, 403, 403, 403, 303}, true},
		{"GET /projects/{project}", "/projects/{proj}", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET /projects/{project}/workspaces", "/projects/{proj}/workspaces", "", onUI, []int{302, 404, 200, 200, 200}, false},
		// settings pages: admins only, as only they save them
		{"GET /projects/{project}/settings", "/projects/{proj}/settings", "", onUI, []int{302, 404, 403, 403, 200}, false},
		{"GET /projects/{project}/settings/{page}", "/projects/{proj}/settings/access", "", onUI, []int{302, 404, 403, 403, 200}, false},
		{"POST /projects/{project}/settings", "/projects/{proj}/settings", "name={proj}&description=d", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST /projects/{project}/access", "/projects/{proj}/access", "group=acc-other&level=read", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST /projects/{project}/delete", "/projects/{proj}/delete", "", onUI, []int{401, 403, 403, 403, 409}, true},

		// UI: workspaces. 409 for operators: past the access check, the fixture's queued run is in the way.
		// A project the user can't create in is a field error (422), not a not-found: the operator has
		// operate on the fixture's project, the reader only read. Settings pages need operate.
		{"GET /workspaces", "/workspaces", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"GET /workspaces/new", "/workspaces/new", "", onUI, []int{302, 200, 200, 200, 200}, false},
		{"POST /workspaces/new", "/workspaces/new", "type=cli&ops-workspace={ws}-new&ops-project={proj}", onUI, []int{401, 422, 422, 303, 303}, true},
		{"GET " + w, ws, "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/runs", ws + "/runs", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/states", "/workspaces/{cws}/states", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/variables", ws + "/variables", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/settings", ws + "/settings", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"GET " + w + "/deploy", ws + "/deploy", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/settings/{page}", ws + "/settings/access", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"GET " + w + "/states/{version}", "/workspaces/{cws}/states/{sv}", "", onUI, []int{302, 404, 403, 200, 200}, false}, // values: operators only
		{"GET " + w + "/vcs", ws + "/vcs", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"GET " + w + "/vcs/edit", ws + "/vcs/edit", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"POST " + w + "/vcs", ws + "/vcs", "", onUI, []int{401, 404, 403, 303, 303}, false},
		{"GET " + w + "/templates/new", ws + "/templates/new?template={tpl}", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"POST " + w + "/templates/new", ws + "/templates/new?template={tpl}", "location=northeurope", onUI, []int{401, 404, 403, 409, 409}, true},
		{"GET " + w + "/templates/{module}", ws + "/templates/{module}", "", onUI, []int{302, 404, 403, 200, 200}, false},
		{"POST " + w + "/templates/{module}", ws + "/templates/{module}", "location=northeurope", onUI, []int{401, 404, 403, 409, 409}, true},
		{"POST " + w + "/templates/{module}/remove", ws + "/templates/{module}/remove", "", onUI, []int{401, 404, 403, 409, 409}, true},
		{"POST " + w + "/variables", ws + "/variables", "category=terraform&key=k2&value=v", onUI, []int{401, 404, 403, 303, 303}, true},
		{"POST " + w + "/variables/{id}/delete", ws + "/variables/{var}/delete", "", onUI, []int{401, 404, 403, 303, 303}, true},
		{"POST " + w + "/settings", ws + "/settings", "name={ws}&execution_mode=remote", onUI, []int{401, 404, 403, 303, 303}, true},
		{"POST " + w + "/settings/health", ws + "/settings/health", "drift_detection=on", onUI, []int{401, 404, 403, 303, 303}, false},
		{"POST " + w + "/settings/remote-state", ws + "/settings/remote-state", "remote_state=specific", onUI, []int{401, 404, 403, 303, 303}, false},
		{"POST " + w + "/access", ws + "/access", "group=acc-other&level=read", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST " + w + "/lock", ws + "/lock", "action=lock", onUI, []int{401, 404, 403, 303, 303}, true},
		{"POST " + w + "/project", ws + "/project", "project={proj}", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST " + w + "/state-versions/{version}/rollback", "/workspaces/{cws}/state-versions/{sv}/rollback", "", onUI, []int{401, 404, 403, 409, 409}, true},
		{"POST " + w + "/runs", ws + "/runs", "action=plan", onUI, []int{401, 404, 403, 409, 409}, true},
		{"GET " + w + "/runs/{run}", ws + "/runs/{run}", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/runs/{run}/logs", ws + "/runs/{run}/logs", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"GET " + w + "/runs/{run}/logs/stream", ws + "/runs/{run}/logs/stream", "", onUI, []int{302, 404, 200, 200, 200}, false},
		{"POST " + w + "/runs/{run}/confirm", ws + "/runs/{run}/confirm", "", onUI, []int{401, 404, 403, 409, 409}, true},
		{"POST " + w + "/runs/{run}/discard", ws + "/runs/{run}/discard", "", onUI, []int{401, 404, 403, 409, 409}, true},
		{"POST " + w + "/runs/{run}/cancel", ws + "/runs/{run}/cancel", "", onUI, []int{401, 404, 403, 303, 303}, true},
		{"POST " + w + "/delete", ws + "/delete", "", onUI, []int{401, 404, 403, 409, 409}, true},

		// UI: variable sets.
		{"GET /variable-sets", "/variable-sets", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /variable-sets", "/variable-sets", "name={set}-new", onUI, []int{401, 403, 403, 403, 303}, true},
		{"GET /variable-sets/{set}", "/variable-sets/{set}", "", onUI, []int{302, 403, 403, 403, 200}, false},
		{"POST /variable-sets/{set}", "/variable-sets/{set}", "description=d", onUI, []int{401, 403, 403, 403, 303}, false},
		{"POST /variable-sets/{set}/variables", "/variable-sets/{set}/variables", "category=env&key=K2&value=v", onUI, []int{401, 403, 403, 403, 303}, true},
		{"POST /variable-sets/{set}/variables/{id}/delete", "/variable-sets/{set}/variables/{setvar}/delete", "", onUI, []int{401, 403, 403, 403, 303}, true},
		{"POST /variable-sets/{set}/workspaces", "/variable-sets/{set}/workspaces", "workspace={ws}", onUI, []int{401, 403, 403, 403, 303}, true},
		{"POST /variable-sets/{set}/projects", "/variable-sets/{set}/projects", "project={proj}", onUI, []int{401, 403, 403, 403, 303}, true},
		{"POST /variable-sets/{set}/delete", "/variable-sets/{set}/delete", "", onUI, []int{401, 403, 403, 403, 303}, true},

		// Last: revokes the users' terraform login tokens, which the cloud API rows and fixtures use.
		{"POST /account/tokens/revoke", "/account/tokens/revoke", "", onUI, []int{401, 303, 303, 303, 303}, false},
	}
}
