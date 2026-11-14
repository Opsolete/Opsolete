package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/auth/authtest"
)

// TestAPIPlatform: what a Terraform provider does, over HTTP with a service token: create, read (by id, and by name
// in a list), change and delete projects, empty workspaces, variables, variable sets and their attachments; a
// sensitive value is never returned; fixed fields are refused (422); a revoked token is refused (401).
func TestAPIPlatform(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	is := authtest.New(t)
	oidc, err := auth.New(ctx, auth.Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		PublicURL: testReg.PublicURL}, []byte("k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newApp(t, "../../catalog", store, oidc, Features{Deployments: true, Registry: true}).Handler()
	sfx := strings.ToLower(newID()[:8])
	tokID, token, err := store.CreateServiceToken(ctx, "tf-"+sfx, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := do(h, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	list := func(path string) []any {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		var out []any
		json.Unmarshal(do(h, req).Body.Bytes(), &out)
		return out
	}

	// Projects.
	proj := call("POST", "/projects", `{"name":"prd-`+sfx+`","description":"production","runner_pool":"prd"}`, http.StatusCreated)
	pid := proj["id"].(string)
	if proj["runner_pool"] != "prd" || proj["default"] != false {
		t.Fatalf("project: %v", proj)
	}
	if got := call("PATCH", "/projects/"+pid, `{"description":"prd"}`, http.StatusOK); got["description"] != "prd" || got["runner_pool"] != "prd" {
		t.Fatalf("patched project (the rest kept): %v", got)
	}
	if l := list("/projects?name=prd-" + sfx); len(l) != 1 || l[0].(map[string]any)["id"] != pid {
		t.Fatalf("by name: %v", l)
	}

	// An empty workspace in it, its settings, moved.
	ws := call("POST", "/workspaces", `{"name":"shop-`+sfx+`","project_id":"`+pid+`","drift_detection":true}`, http.StatusCreated)
	wid := ws["id"].(string)
	if ws["project_id"] != pid || ws["drift_detection"] != true || ws["execution_mode"] != "remote" || ws["latest_run"] != nil {
		t.Fatalf("workspace: %v", ws)
	}
	ws = call("PATCH", "/workspaces/"+wid, `{"name":"shop2-`+sfx+`","auto_apply":true,"drift_detection":false}`, http.StatusOK)
	if ws["name"] != "shop2-"+sfx || ws["auto_apply"] != true || ws["drift_detection"] != false || ws["id"] != wid {
		t.Fatalf("patched workspace (renamed: same id): %v", ws)
	}
	other := call("POST", "/projects", `{"name":"dev-`+sfx+`"}`, http.StatusCreated)["id"].(string)
	if ws = call("PATCH", "/workspaces/"+wid, `{"project_id":"`+other+`"}`, http.StatusOK); ws["project_id"] != other {
		t.Fatalf("moved: %v", ws)
	}

	// Its variables: a sensitive value is write-only; category and key are fixed.
	v := call("POST", "/workspaces/"+wid+"/variables", `{"category":"env","key":"TOKEN","value":"s3cret","sensitive":true}`, http.StatusCreated)
	vid := v["id"].(string)
	if v["value"] != nil || v["sensitive"] != true {
		t.Fatalf("sensitive variable: %v", v)
	}
	call("POST", "/workspaces/"+wid+"/variables", `{"category":"env","key":"TOKEN","value":"x"}`, http.StatusConflict)
	call("PATCH", "/workspaces/"+wid+"/variables/"+vid, `{"sensitive":true}`, http.StatusUnprocessableEntity) // no value: can't keep it
	call("PATCH", "/workspaces/"+wid+"/variables/"+vid, `{"key":"OTHER","value":"x"}`, http.StatusUnprocessableEntity)
	if v = call("PATCH", "/workspaces/"+wid+"/variables/"+vid, `{"value":"plain","sensitive":false}`, http.StatusOK); v["value"] != "plain" || v["id"] != vid {
		t.Fatalf("patched variable (same id): %v", v)
	}
	call("DELETE", "/workspaces/"+wid+"/variables/"+vid, "", http.StatusNoContent)
	call("GET", "/workspaces/"+wid+"/variables/"+vid, "", http.StatusNotFound)

	// A variable set, its variable, attached to the project and the workspace.
	set := call("POST", "/variable-sets", `{"name":"azure-`+sfx+`","priority":true}`, http.StatusCreated)
	sid := set["id"].(string)
	if set["priority"] != true || set["global"] != false {
		t.Fatalf("set: %v", set)
	}
	call("PATCH", "/variable-sets/"+sid, `{"name":"renamed"}`, http.StatusUnprocessableEntity)
	call("POST", "/variable-sets/"+sid+"/variables", `{"category":"terraform","key":"tags","value":"{env = \"prd\"}","hcl":true}`, http.StatusCreated)
	call("PUT", "/variable-sets/"+sid+"/projects/"+pid, "", http.StatusNoContent)
	call("PUT", "/variable-sets/"+sid+"/projects/"+pid, "", http.StatusNoContent) // idempotent
	call("PUT", "/variable-sets/"+sid+"/workspaces/"+wid, "", http.StatusNoContent)
	set = call("GET", "/variable-sets/"+sid, "", http.StatusOK)
	if p, w := set["project_ids"].([]any), set["workspace_ids"].([]any); len(p) != 1 || p[0] != pid || len(w) != 1 || w[0] != wid {
		t.Fatalf("attachments: %v", set)
	}
	call("DELETE", "/variable-sets/"+sid+"/projects/"+pid, "", http.StatusNoContent)
	if set = call("GET", "/variable-sets/"+sid, "", http.StatusOK); len(set["project_ids"].([]any)) != 0 {
		t.Fatalf("detached: %v", set)
	}

	// Deleting: the workspace, the set, the projects (empty now).
	call("DELETE", "/workspaces/"+wid, "", http.StatusNoContent)
	call("DELETE", "/variable-sets/"+sid, "", http.StatusNoContent)
	call("DELETE", "/projects/"+pid, "", http.StatusNoContent)
	call("DELETE", "/projects/"+other, "", http.StatusNoContent)
	call("GET", "/projects/"+pid, "", http.StatusNotFound)

	// The audit log names the token; a revoked token is refused.
	var actor string
	store.Pool.QueryRow(ctx, `SELECT actor FROM audit_log WHERE action = 'DELETE /api/v1/projects/'||$1 ORDER BY id DESC LIMIT 1`, other).Scan(&actor)
	if actor != "token:tf-"+sfx {
		t.Fatalf("audited as %q", actor)
	}
	if err := store.DeleteServiceToken(ctx, tokID); err != nil {
		t.Fatal(err)
	}
	call("GET", "/projects", "", http.StatusUnauthorized)
}
