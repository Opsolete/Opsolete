package httpserver

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rudesome/form2stack/internal/storage"
)

func TestCloudLoginAndState(t *testing.T) {
	store := testStore(t)
	email := fmt.Sprintf("cli-%s@example.com", newID()[:8])
	app := newApp(t, "../../catalog", store, devAuth(t, store, email, false), Features{Deployments: true, Registry: true})
	app.Organization = "acme"
	h := app.Handler()

	// terraform login: authorize (PKCE S256), then exchange the code with the verifier.
	verifier := "test-verifier-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {"terraform-cli"}, "response_type": {"code"}, "state": {"st"},
		"redirect_uri": {"http://localhost:10000/login"}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example.com/login")
	if rec := post(h, "/oauth/authorization?"+bad.Encode(), nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign redirect_uri: %d", rec.Code)
	}
	rec := post(h, "/oauth/authorization?"+q.Encode(), nil)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || loc.Host != "localhost:10000" || loc.Query().Get("state") != "st" {
		t.Fatalf("authorize: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {"http://localhost:10000/login"}, "code_verifier": {"wrong"}}
	if rec := post(h, "/oauth/token", exchange); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong verifier: %d", rec.Code)
	}
	exchange.Set("code_verifier", verifier)
	rec = post(h, "/oauth/token", exchange)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &tok); rec.Code != 200 || tok.AccessToken == "" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}

	api := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, cloudPrefix+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		return do(h, req)
	}
	if rec := api("GET", "/organizations/other/entitlement-set", ""); rec.Code != 404 {
		t.Fatalf("other org: %d", rec.Code)
	}
	if rec := api("GET", "/organizations/acme/entitlement-set", ""); !strings.Contains(rec.Body.String(), `"operations":false`) {
		t.Fatalf("entitlements: %s", rec.Body)
	}
	name := "cli-" + newID()[:8]
	rec = api("POST", "/organizations/acme/workspaces", `{"data":{"type":"workspaces","attributes":{"name":"`+name+`"}}}`)
	var ws struct{ Data apiObject }
	if json.Unmarshal(rec.Body.Bytes(), &ws); rec.Code != 201 || ws.Data.Attributes["execution-mode"] != "local" {
		t.Fatalf("create workspace: %d %s", rec.Code, rec.Body)
	}
	id := ws.Data.ID
	if rec := api("GET", "/workspaces/"+id+"/current-state-version", ""); rec.Code != 404 {
		t.Fatalf("no state yet: %d", rec.Code)
	}

	state := `{"version":4,"serial":1,"lineage":"l-1","outputs":{"pw":{"value":"x","type":"string","sensitive":true}},"resources":[]}`
	m := md5.Sum([]byte(state))
	sv := fmt.Sprintf(`{"data":{"type":"state-versions","attributes":{"serial":1,"md5":%q,"state":%q}}}`,
		hex.EncodeToString(m[:]), base64.StdEncoding.EncodeToString([]byte(state)))
	if rec := api("POST", "/workspaces/"+id+"/state-versions", `{"data":{"attributes":{"serial":1}}}`); rec.Code != 422 ||
		!strings.Contains(rec.Body.String(), "param is missing or the value is empty: state") {
		t.Fatalf("upload-URL flow must fall back: %d %s", rec.Code, rec.Body)
	}
	if rec := api("POST", "/workspaces/"+id+"/state-versions", sv); rec.Code != http.StatusConflict {
		t.Fatalf("save without lock: %d", rec.Code)
	}
	if rec := api("POST", "/workspaces/"+id+"/actions/lock", ""); rec.Code != 200 {
		t.Fatalf("lock: %d", rec.Code)
	}
	if rec := api("POST", "/workspaces/"+id+"/actions/lock", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second lock: %d", rec.Code)
	}
	if rec := api("POST", "/workspaces/"+id+"/state-versions", sv); rec.Code != 201 {
		t.Fatalf("save state: %d %s", rec.Code, rec.Body)
	}
	if rec := api("POST", "/workspaces/"+id+"/actions/unlock", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"locked":false`) {
		t.Fatalf("unlock: %d %s", rec.Code, rec.Body)
	}
	rec = api("GET", "/workspaces/"+id+"/current-state-version", "")
	var cur struct{ Data apiObject }
	json.Unmarshal(rec.Body.Bytes(), &cur)
	dl, _ := cur.Data.Attributes["hosted-state-download-url"].(string)
	if rec := api("GET", strings.TrimPrefix(dl, cloudPrefix), ""); rec.Body.String() != state {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	if out := api("GET", "/workspaces/"+id+"/current-state-version-outputs", "").Body.String(); !strings.Contains(out, `"name":"pw"`) || strings.Contains(out, `"x"`) {
		t.Fatalf("outputs must list names, not values: %s", out)
	}

	// Local execution starts no runs; remote execution gives the CLI no lock.
	if _, err := store.CreateRun(t.Context(), id, "apply", "", nil); !errors.Is(err, storage.ErrLocalExecution) {
		t.Fatalf("run on a local workspace: %v", err)
	}
	if err := store.SetExecutionMode(t.Context(), id, "remote"); err != nil {
		t.Fatal(err)
	}
	if rec := api("POST", "/workspaces/"+id+"/actions/lock", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "remote execution") {
		t.Fatalf("lock of a remote workspace: %d %s", rec.Code, rec.Body)
	}

	// Another (non-admin) user's workspace is invisible.
	other, err := store.CreateEmptyWorkspace(t.Context(), "cli-other-"+newID()[:8], false, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec := api("POST", "/workspaces/"+other.ID+"/actions/lock", ""); rec.Code != 404 {
		t.Fatalf("lock without access: %d", rec.Code)
	}
	if rec := do(h, httptest.NewRequest("GET", cloudPrefix+"/organizations/acme/workspaces/"+name, nil)); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
}
