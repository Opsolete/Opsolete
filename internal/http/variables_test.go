package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// TestTeamsVariablesRenameCancel covers workspace access by IdP group, variables and variable
// sets handed to runners, renaming and canceling, through the HTTP routes.
func TestTeamsVariablesRenameCancel(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	os.WriteFile(dir+"/vars.json", []byte(`{"id":"vars","name":"Vars","module":{"source":"git::https://x//m","ref":"v1"},
		"inputs":[{"name":"location","type":"string","default":"westeurope"}]}`), 0o600)
	admin := newServerAs(t, dir, false, store, devAuth(t, store, "admin@example.com", true))
	operateDefault(t, store, "grp-owners") // where the owner creates the workspace
	owner := newServerAs(t, dir, false, store, devAuth(t, store, "owner@example.com", false, "grp-owners"))
	reader := newServerAs(t, dir, false, store, devAuth(t, store, "reader@example.com", false, "grp-readers"))
	operator := newServerAs(t, dir, false, store, devAuth(t, store, "op@example.com", false, "grp-ops"))
	outsider := newServerAs(t, dir, false, store, devAuth(t, store, "out@example.com", false, "grp-other"))

	name := "tp-" + strings.ToLower(newID()[:8])
	setName := name + "-set"
	runnerTok := testRunnerToken
	var (
		runURL string // the workspace's first run
		job    runnerapi.Job
	)
	step(t, "owner creates a workspace from a template", func(t *testing.T) {
		rec := post(owner, "/templates/vars", url.Values{"ops-workspace": {name}, "location": {"westeurope"}})
		runURL = rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
	})
	wsURL := "/workspaces/" + name

	// Visibility: owner and admin; groups only once granted, and read is read-only.
	step(t, "groups see the workspace once granted, and read access is read-only", func(t *testing.T) {
		for who, h := range map[string]http.Handler{"reader": reader, "operator": operator, "outsider": outsider} {
			if rec := get(h, wsURL); rec.Code != http.StatusNotFound || strings.Contains(get(h, "/workspaces").Body.String(), name) {
				t.Fatalf("%s sees an ungranted workspace: %d", who, rec.Code)
			}
		}
		if rec := post(owner, wsURL+"/access", url.Values{"group": {"grp-readers"}, "level": {"read"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("only admins grant access: %d", rec.Code)
		}
		for group, level := range map[string]string{"grp-readers": "read", "grp-ops": "operate"} {
			if rec := post(admin, wsURL+"/access", url.Values{"group": {group}, "level": {level}}); rec.Code != http.StatusSeeOther {
				t.Fatalf("grant %s: %d", group, rec.Code)
			}
		}
		if !strings.Contains(get(reader, "/workspaces").Body.String(), name) || get(reader, runURL).Code != 200 {
			t.Fatal("a read grant must show the workspace and its runs")
		}
		if !strings.Contains(get(admin, wsURL+"/settings/access").Body.String(), `<option value="grp-ops">`) {
			t.Fatal("grant form must offer the groups seen at login")
		}
		if rec := get(owner, "/workspaces/new?template=vars"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/templates/vars" ||
			!strings.Contains(get(owner, "/workspaces/new?type=template").Body.String(), `<option value="vars">`) {
			t.Fatalf("new workspace can start from a template: %d", rec.Code)
		}
		if rec := post(reader, wsURL+"/runs", url.Values{"action": {"destroy"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("read access must not start runs: %d", rec.Code)
		}
		if page := get(reader, runURL).Body.String(); strings.Contains(page, "Cancel run") {
			t.Fatal("cancel button shown to a reader")
		}
		if get(outsider, wsURL).Code != http.StatusNotFound {
			t.Fatal("other groups still see nothing")
		}
	})

	// Variables: workspace ones by operators, sets by admins; sensitive values never shown again.
	step(t, "operators set workspace variables, admins variable sets; sensitive values stay hidden", func(t *testing.T) {
		for _, v := range []url.Values{
			{"category": {"env"}, "key": {"ARM_CLIENT_SECRET"}, "value": {"s3cret-ws"}, "sensitive": {"on"}},
			{"category": {"env"}, "key": {"ARM_CLIENT_ID"}, "value": {"client-ws"}},
			{"category": {"env"}, "key": {"OPS_DEBUG"}, "value": {"1"}}, // the one OPS_* a workspace may set
			{"category": {"terraform"}, "key": {"tags"}, "value": {"from-ws"}},
		} {
			if rec := post(operator, wsURL+"/variables", v); rec.Code != http.StatusSeeOther {
				t.Fatalf("set %v: %d %s", v, rec.Code, rec.Body.String())
			}
		}
		for _, v := range []url.Values{
			{"category": {"env"}, "key": {"PATH"}, "value": {"/tmp"}},
			{"category": {"env"}, "key": {"TF_CLI_ARGS"}, "value": {"-x"}},
			{"category": {"env"}, "key": {"TF_VAR_tags"}, "value": {"x"}},
			{"category": {"env"}, "key": {"GIT_SSH_COMMAND"}, "value": {"sh"}},
			{"category": {"env"}, "key": {"OPS_RUNNER_TOKEN"}, "value": {"x"}},
			{"category": {"env"}, "key": {"ops_debug"}, "value": {"1"}}, // the runner reads OPS_DEBUG only
			{"category": {"nope"}, "key": {"A"}},
		} {
			if rec := post(operator, wsURL+"/variables", v); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%v must be rejected: %d", v, rec.Code)
			}
		}
		if rec := post(reader, wsURL+"/variables", url.Values{"category": {"env"}, "key": {"A"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("a reader must not set variables: %d", rec.Code)
		}
		if rec := post(operator, "/variable-sets", url.Values{"name": {setName}}); rec.Code != http.StatusForbidden {
			t.Fatalf("only admins manage variable sets: %d", rec.Code)
		}
		for _, c := range []struct {
			path string
			form url.Values
		}{
			{"/variable-sets", url.Values{"name": {setName}}},
			{"/variable-sets/" + setName + "/variables", url.Values{"category": {"env"}, "key": {"ARM_TENANT_ID"}, "value": {"tenant-set"}}},
			{"/variable-sets/" + setName + "/workspaces", url.Values{"workspace": {name}}},
		} {
			if rec := post(admin, c.path, c.form); rec.Code != http.StatusSeeOther {
				t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Body.String())
			}
		}
		// A second set entry with the workspace's key: the workspace's own variable wins.
		post(admin, "/variable-sets/"+setName+"/variables", url.Values{"category": {"env"}, "key": {"ARM_CLIENT_ID"}, "value": {"client-set"}})
		if rec := post(admin, "/variable-sets/"+setName+"/workspaces", url.Values{"workspace": {"no-such-ws"}}); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("attach to an unknown workspace: %d", rec.Code)
		}
		page := get(reader, wsURL+"/variables").Body.String()
		if strings.Contains(page, "s3cret-ws") || !strings.Contains(page, "client-ws") || !strings.Contains(page, setName) ||
			!strings.Contains(page, "tenant-set") {
			t.Fatalf("workspace page must show non-sensitive values and applied sets, never sensitive ones: %s", page)
		}
		if p := get(admin, "/variable-sets/"+setName).Body.String(); strings.Contains(p, "<option>"+name+"</option>") {
			t.Fatal("an attached workspace must not be offered again")
		}
		if p := get(admin, "/variable-sets/"+setName).Body.String(); strings.Contains(p, "s3cret") || !strings.Contains(p, "tenant-set") {
			t.Fatalf("set page must show non-sensitive values only: %s", p)
		}
		if p := get(admin, "/variable-sets").Body.String(); !strings.Contains(p, setName) || !strings.Contains(p, "</html>") {
			t.Fatalf("variable sets list: %s", p)
		}
	})

	// Any runner gets the run, with the variables.
	step(t, "the runner's job carries the workspace's and its sets' variables", func(t *testing.T) {
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r"}`))
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		rec := do(admin, req)
		if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || !strings.HasSuffix(runURL, job.RunID) {
			t.Fatalf("claim: %d %s", rec.Code, rec.Body.String())
		}
		req = httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/"+job.RunID+"/ack", nil)
		req.Header.Set("Authorization", "Bearer "+runnerTok)
		req.Header.Set(runnerapi.LeaseHeader, job.LeaseID)
		if rec := do(admin, req); rec.Code != http.StatusNoContent {
			t.Fatalf("ack: %d", rec.Code)
		}
		if job.Env["ARM_CLIENT_SECRET"] != "s3cret-ws" || job.Env["ARM_CLIENT_ID"] != "client-ws" || job.Env["ARM_TENANT_ID"] != "tenant-set" ||
			job.Env["OPS_DEBUG"] != "1" || job.Vars["tags"] != "from-ws" || len(job.Modules) != 1 || job.Modules[0].Inputs["location"] != "westeurope" {
			t.Fatalf("job variables: env %v vars %v modules %+v", job.Env, job.Vars, job.Modules)
		}
	})

	// Cancel: an operator asks; the runner hears it on its next heartbeat and keeps its lease.
	step(t, "cancel reaches the runner on its heartbeat, force cancel cuts it off", func(t *testing.T) {
		beat := func() int {
			req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/"+job.RunID+"/heartbeat", nil)
			req.Header.Set("Authorization", "Bearer "+runnerTok)
			req.Header.Set(runnerapi.LeaseHeader, job.LeaseID)
			return do(admin, req).Code
		}
		if c := beat(); c != http.StatusNoContent {
			t.Fatalf("heartbeat before cancel: %d", c)
		}
		if !strings.Contains(get(operator, runURL).Body.String(), "Cancel run") {
			t.Fatal("cancel button missing for an operator")
		}
		if rec := post(operator, runURL+"/cancel", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("cancel: %d", rec.Code)
		}
		if c := beat(); c != runnerapi.StatusCancel {
			t.Fatalf("the runner must be asked to cancel: %d", c)
		}
		if !strings.Contains(get(operator, runURL).Body.String(), "Force cancel") {
			t.Fatal("force cancel must be offered once a cancel is pending")
		}
		if rec := post(operator, runURL+"/cancel", url.Values{"force": {"1"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("force cancel: %d", rec.Code)
		}
		if c := beat(); c != http.StatusConflict {
			t.Fatalf("a force-canceled runner must be cut off: %d", c)
		}
		if rec := post(operator, runURL+"/cancel", nil); rec.Code != http.StatusConflict {
			t.Fatalf("canceling a finished run: %d", rec.Code)
		}
	})

	// Rename: same workspace, new URL; the old one is gone; grants and runs stay.
	step(t, "rename keeps the workspace's runs, grants and variables", func(t *testing.T) {
		if rec := post(reader, wsURL+"/settings", url.Values{"name": {name + "-renamed"}, "execution_mode": {"remote"}}); rec.Code != http.StatusForbidden {
			t.Fatalf("a reader must not rename: %d", rec.Code)
		}
		if rec := post(operator, wsURL+"/settings", url.Values{"name": {"Not Valid"}, "execution_mode": {"remote"}}); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid name: %d", rec.Code)
		}
		if rec := post(operator, wsURL+"/settings", url.Values{"name": {name + "-renamed"}, "execution_mode": {"remote"}}); rec.Code != http.StatusSeeOther ||
			rec.Header().Get("Location") != wsURL+"-renamed/settings" {
			t.Fatalf("rename: %d %q", rec.Code, rec.Header().Get("Location"))
		}
		if get(owner, wsURL).Code != http.StatusNotFound {
			t.Fatal("the old name must be free")
		}
		page := get(reader, wsURL+"-renamed/runs").Body.String() + get(reader, wsURL+"-renamed/variables").Body.String()
		if !strings.Contains(page, "canceled") || !strings.Contains(page, "client-ws") || !strings.Contains(page, setName) {
			t.Fatalf("renamed workspace lost its runs or variables: %s", page)
		}
	})
}
