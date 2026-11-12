package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPI: the JSON API creates a workspace from a template with typed inputs, shows the plan's attribute changes,
// confirms it, and answers errors as {"error", "fields"} with the UI's statuses.
func TestAPI(t *testing.T) {
	_, _, h, r := flowServer(t)
	call := func(method, path, body string, want int) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := do(h, req)
		if rec.Code != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	ws := "api-" + strings.ToLower(newID()[:8])

	tpl := call("GET", "/api/v1/templates/greeting", "", http.StatusOK)
	fields, _ := tpl["form"].(map[string]any)["fields"].([]any)
	if len(fields) == 0 || fields[0].(map[string]any)["name"] == "" {
		t.Fatalf("form: %v", tpl)
	}

	bad := call("POST", "/api/v1/workspaces", `{"name": "Bad Name", "template": "greeting", "inputs": {"greeting": "hi"}}`, http.StatusUnprocessableEntity)
	if f, _ := bad["fields"].(map[string]any); f["name"] == nil || f["inputs.name"] == nil {
		t.Fatalf("validation error: %v", bad)
	}
	call("POST", "/api/v1/workspaces", `{"name": "x", "bogus": 1}`, http.StatusBadRequest)

	created := call("POST", "/api/v1/workspaces", `{"name": "`+ws+`", "template": "greeting", "inputs": {"greeting": "hi", "name": "api"}}`, http.StatusCreated)
	run, _ := created["latest_run"].(map[string]any)["id"].(string)
	if run == "" {
		t.Fatalf("created: %v", created)
	}
	r.claim(t, run, "plan")
	r.finish(t, `{"status": "succeeded", "plan": {"add": 1, "change": 0, "destroy": 0, "changes": [{"address": "x.y", "action": "create"}]},
		"diff": [{"address": "x.y", "action": "create", "attrs": [{"path": "input", "after": "\"hi, api!\"", "has_after": true}]}]}`)
	got := call("GET", "/api/v1/workspaces/"+ws+"/runs/"+run, "", http.StatusOK)
	if got["status"] != "needs_confirmation" || got["confirm_reason"] != "manual" || len(got["history"].([]any)) < 2 {
		t.Fatalf("run: %v", got)
	}
	if d, _ := got["diff"].([]any); len(d) != 1 || !strings.Contains(mustJSON(d), `"hi, api!`) {
		t.Fatalf("diff: %v", got["diff"])
	}
	call("POST", "/api/v1/workspaces/"+ws+"/runs/"+run+"/confirm", "", http.StatusNoContent)
	call("POST", "/api/v1/workspaces/"+ws+"/runs/"+run+"/confirm", "", http.StatusConflict) // not waiting anymore

	w := call("GET", "/api/v1/workspaces/"+ws, "", http.StatusOK)
	if tpls, _ := w["templates"].([]any); len(tpls) != 1 || !strings.Contains(mustJSON(tpls), `"api"`) {
		t.Fatalf("workspace: %v", w)
	}
	call("GET", "/api/v1/workspaces/nope-"+ws, "", http.StatusNotFound)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
