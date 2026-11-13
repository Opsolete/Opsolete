package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// TestWorkspaceFlow follows one workspace from its creation through plan, confirmation, apply,
// replace, plan-only and drift runs, destroy and a state rollback, as the UI, the runner API and
// Terraform's http backend call it. Each step needs the state the ones before it left.
func TestWorkspaceFlow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true})
	h := srv.Handler()
	other := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "other@example.com", false))
	name := "wf-" + strings.ToLower(newID()[:8])
	var (
		runURL, runID string // the run under test
		ws            storage.Workspace
		job           runnerapi.Job // the last claimed job
	)

	runnerTok := testRunnerToken
	registryTok, _ := store.CreateToken(ctx, "reg", storage.ScopeRegistry)
	call := func(method, token, path, lease, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, runnerapi.Prefix+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(runnerapi.LeaseHeader, lease)
		return do(h, req)
	}
	stateCall := func(method, query, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, runnerapi.StatePrefix+"/"+ws.ID.String()+query, strings.NewReader(body))
		if token != "" {
			req.SetBasicAuth(ws.ID.String(), token)
		}
		return do(h, req)
	}
	claimJob := func(t *testing.T, phase string) {
		t.Helper()
		rec := call("POST", runnerTok, "/jobs/claim", "", `{"runner":"r1"}`)
		if json.Unmarshal(rec.Body.Bytes(), &job); rec.Code != 200 || job.RunID != runID || job.WorkspaceID != ws.ID.String() || job.Phase != phase {
			t.Fatalf("claim %s: %d %s", phase, rec.Code, rec.Body.String())
		}
		// the runner acknowledges the job before anything else: until then its lease is refused
		if rec := call("POST", runnerTok, "/jobs/"+job.RunID+"/logs", job.LeaseID, "too early\n"); rec.Code != http.StatusConflict {
			t.Fatalf("log before the ack: %d, want 409", rec.Code)
		}
		if rec := stateCall("GET", "", job.StateToken, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("state before the ack: %d, want 403", rec.Code)
		}
		if rec := call("POST", runnerTok, "/jobs/"+job.RunID+"/ack", job.LeaseID, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("ack %s: %d %s", phase, rec.Code, rec.Body.String())
		}
	}
	noJob := func(t *testing.T, why string) {
		t.Helper()
		if rec := call("POST", runnerTok, "/jobs/claim", "", `{"runner":"r2"}`); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: claim got %d", why, rec.Code)
		}
	}
	state := func(serial int, lineage, resource string) string {
		return fmt.Sprintf(`{"version":4,"serial":%d,"lineage":%q,"resources":[%s]}`, serial, lineage, resource)
	}
	saveState := func(t *testing.T, body string) {
		t.Helper()
		for _, c := range [][3]string{{"LOCK", "", `{"ID":"x1"}`}, {"POST", "?ID=x1", body}, {"UNLOCK", "", `{"ID":"x1"}`}} {
			if rec := stateCall(c[0], c[1], job.StateToken, c[2]); rec.Code != http.StatusOK {
				t.Fatalf("save state %s: %d %s", c[0], rec.Code, rec.Body.String())
			}
		}
	}
	finish := func(t *testing.T, body string) {
		t.Helper()
		if rec := call("POST", runnerTok, "/jobs/"+runID+"/finish", job.LeaseID, body); rec.Code != http.StatusNoContent {
			t.Fatalf("finish %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	type stateCase struct {
		method, query, token, body string
		want                       int
	}
	stateCases := func(t *testing.T, cases []stateCase) {
		t.Helper()
		for _, c := range cases {
			if rec := stateCall(c.method, c.query, c.token, c.body); rec.Code != c.want {
				t.Fatalf("%s%s %s: %d, want %d: %s", c.method, c.query, c.body, rec.Code, c.want, rec.Body.String())
			}
		}
	}
	// bucket read access alone must not reveal what the runner uploaded
	assertSealed := func(t *testing.T, key, secret string) {
		t.Helper()
		raw, err := srv.App.Blobs.ReadAll(ctx, key)
		if err != nil || !bytes.HasPrefix(raw, []byte(app.SealedPrefix)) || bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s not sealed: %q %v", key, raw, err)
		}
	}
	planPath := func() string { return "/jobs/" + runID + "/artifacts/" + runnerapi.ArtifactPlan }
	lockPath := func() string { return "/jobs/" + runID + "/artifacts/" + runnerapi.ArtifactLockfile }
	v1 := state(1, "lin", `{"mode":"managed","type":"azurerm_storage_account","name":"this"}`)
	var planToken string           // the run's state password, from its plan phase
	var v1s []storage.StateVersion // the apply's intermediate state version

	step(t, "create workspace from a template", func(t *testing.T) {
		form := url.Values{"name": {"acct01"}, "location": {"westeurope"}, "retention_days": {"7"}, "ops-workspace": {"Bad Name"}}
		if rec := post(h, "/templates/storage-account", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "lowercase letters") {
			t.Fatalf("invalid workspace name: %d", rec.Code)
		}
		form.Set("ops-workspace", name)
		rec := post(h, "/templates/storage-account", form)
		runURL = rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(runURL, "/workspaces/"+name+"/runs/") {
			t.Fatalf("create workspace: %d %q %s", rec.Code, runURL, rec.Body.String())
		}
		runID = strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
		if rec := post(h, "/templates/storage-account", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already exists") {
			t.Fatalf("duplicate workspace name: %d", rec.Code)
		}
		var err error
		if ws, err = store.GetWorkspace(ctx, name); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("only the runner token claims jobs", func(t *testing.T) {
		if rec := call("POST", registryTok, "/jobs/claim", "", `{"runner":"r1"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a registry token must not claim jobs: %d", rec.Code)
		}
		if rec := call("POST", testRunnerToken+"x", "/jobs/claim", "", `{"runner":"r1"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a wrong runner token must not claim jobs: %d", rec.Code)
		}
	})

	step(t, "plan job carries the template's module, once", func(t *testing.T) {
		claimJob(t, "plan")
		if len(job.Modules) != 1 || job.Modules[0].Inputs["name"] != "acct01" || job.Modules[0].Inputs["retention_days"] != 7.0 || job.ModuleSource != "" ||
			job.Modules[0].Source != "git::https://dev.azure.com/your-org/infra/_git/terraform-modules//storage-account?ref=v1.0.0" {
			t.Fatalf("plan job: %+v", job)
		}
		noJob(t, "one job per run phase")
	})

	// State backend, as Terraform's http backend calls it.
	step(t, "a plan locks the state but never writes it", func(t *testing.T) {
		stateCases(t, []stateCase{
			{"GET", "", "", "", http.StatusUnauthorized},
			{"GET", "", "forged", "", http.StatusForbidden},
			{"GET", "", job.StateToken, "", http.StatusNoContent}, // new workspace
			{"LOCK", "", job.StateToken, `{"ID":"l1","Who":"runner"}`, http.StatusOK},
			{"LOCK", "", job.StateToken, `{"ID":"l2"}`, http.StatusLocked},
			{"POST", "?ID=l1", job.StateToken, v1, http.StatusForbidden}, // plans don't write state
			{"UNLOCK", "", job.StateToken, `{"ID":"l1"}`, http.StatusOK},
		})
		stateCall("LOCK", "", job.StateToken, `{"ID":"l3","Who":"alice"}`)
		if rec := stateCall("LOCK", "", job.StateToken, `{"ID":"l4"}`); rec.Code != http.StatusLocked || !strings.Contains(rec.Body.String(), "alice") {
			t.Fatalf("lock conflict body (shows the holder): %d %s", rec.Code, rec.Body.String())
		}
	})

	step(t, "plan phase saves sealed artifacts and logs, then finishes", func(t *testing.T) {
		// The plan phase saves its plan; only the apply phase may read it back.
		for _, c := range []struct {
			method, path, lease string
			want                int
		}{
			{"POST", planPath(), "stolen", http.StatusConflict},
			{"POST", "/jobs/" + runID + "/artifacts/other", job.LeaseID, http.StatusNotFound},
			{"POST", planPath(), job.LeaseID, http.StatusNoContent},
			{"GET", planPath(), job.LeaseID, http.StatusConflict}, // not in the apply phase yet
		} {
			if rec := call(c.method, runnerTok, c.path, c.lease, "PLANBYTES"); rec.Code != c.want {
				t.Fatalf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
			}
		}
		if rec := call("POST", runnerTok, lockPath(), job.LeaseID, "LOCKBYTES"); rec.Code != http.StatusNoContent {
			t.Fatalf("upload lock file: %d", rec.Code)
		}
		assertSealed(t, app.ArtifactKey(uuid.MustParse(runID), runnerapi.ArtifactPlan), "PLANBYTES")
		assertSealed(t, app.ArtifactKey(uuid.MustParse(runID), runnerapi.ArtifactLockfile), "LOCKBYTES")
		for _, chunk := range []string{"line one\n", "line \x1b[1mtwo\x1b[0m\n\nend\n"} {
			if rec := call("POST", runnerTok, "/jobs/"+runID+"/logs", job.LeaseID, chunk); rec.Code != http.StatusNoContent {
				t.Fatalf("logs: %d %s", rec.Code, rec.Body.String())
			}
		}
		if rec := call("POST", runnerTok, "/jobs/"+runID+"/heartbeat", job.LeaseID, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("heartbeat: %d", rec.Code)
		}
		if rec := call("POST", runnerTok, "/jobs/not-an-id/heartbeat", job.LeaseID, ""); rec.Code != http.StatusConflict {
			t.Fatalf("heartbeat of an id that isn't one: %d, want the lease lost as for an unknown run", rec.Code)
		}
		if rec := call("POST", runnerTok, "/jobs/"+runID+"/finish", job.LeaseID, `{"status":"bogus"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid status accepted: %d", rec.Code)
		}
		finish(t, `{"status":"succeeded","plan":{"add":1,"change":0,"destroy":0,"changes":[{"address":"azurerm_storage_account.this","action":"create"}],"cost":{"currency":"USD","prior":0,"proposed":12.5,"delta":12.5,"priced":1}}}`)
		planToken = job.StateToken
	})

	// The plan waits for a human: shown with the changes, and confirmable only by the owner or an admin.
	t.Run("plan page shows the changes and cost", func(t *testing.T) {
		page := get(h, runURL).Body.String()
		for _, want := range []string{"needs confirmation", "1 to add, 0 to change, 0 to destroy", "<code>azurerm_storage_account.this</code>", "Confirm &amp; apply", "Discard", "USD 12.50", "&#43;12.50"} {
			if !strings.Contains(page, want) {
				t.Errorf("run page lacks %q", want)
			}
		}
	})

	step(t, "a plan waiting for confirmation holds the workspace until confirmed", func(t *testing.T) {
		if page := get(other, runURL).Body.String(); strings.Contains(page, "Confirm &amp;") {
			t.Fatal("confirm button shown to a user who may not confirm")
		}
		if rec := post(other, runURL+"/confirm", nil); rec.Code != http.StatusNotFound { // no access: no workspace
			t.Fatalf("confirm by a user without access: %d", rec.Code)
		}
		// a second run waits as pending (HCP's run queue); canceled, it never ran
		rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}})
		pending := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.Contains(get(h, pending).Body.String(), "pending") {
			t.Fatalf("second run while one waits for confirmation: %d", rec.Code)
		}
		if rec := post(h, pending+"/cancel", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("cancel the pending run: %d", rec.Code)
		}
		noJob(t, "a plan waiting for confirmation")
		if rec := stateCall("GET", "", planToken, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("no state access while a plan waits for confirmation: %d", rec.Code)
		}
		if lock, _ := store.StateLockID(ctx, ws.ID); lock != "" {
			t.Fatalf("the plan phase's lock survived it: %q", lock)
		}
		if rec := post(h, runURL+"/confirm", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
		}
	})

	// A new lease, but the run's state password again, because Terraform embeds the backend config
	// (with the password) in the saved plan.
	step(t, "apply phase gets the plan's state password and artifacts", func(t *testing.T) {
		claimJob(t, "apply")
		if job.StateToken != planToken {
			t.Fatal("the apply phase must get the password the saved plan carries")
		}
		if rec := call("GET", runnerTok, planPath(), job.LeaseID, ""); rec.Code != http.StatusOK || rec.Body.String() != "PLANBYTES" {
			t.Fatalf("download plan: %d %q", rec.Code, rec.Body.String())
		}
		if rec := call("GET", runnerTok, lockPath(), job.LeaseID, ""); rec.Code != http.StatusOK || rec.Body.String() != "LOCKBYTES" {
			t.Fatalf("download lock file: %d %q", rec.Code, rec.Body.String())
		}
	})

	step(t, "apply writes state under its lock, newer serials of one lineage only", func(t *testing.T) {
		stateCases(t, []stateCase{
			{"POST", "?ID=l1", job.StateToken, v1, http.StatusConflict}, // not locked
			{"LOCK", "", job.StateToken, `{"ID":"l1","Who":"runner"}`, http.StatusOK},
			{"POST", "?ID=l2", job.StateToken, v1, http.StatusConflict}, // someone else's lock id
			{"POST", "?ID=l1", job.StateToken, `{"not":"state"}`, http.StatusBadRequest},
			{"POST", "?ID=l1", job.StateToken, v1, http.StatusOK},
			{"POST", "?ID=l1", job.StateToken, v1, http.StatusOK},                          // unchanged: no new version
			{"POST", "?ID=l1", job.StateToken, state(0, "lin", ""), http.StatusConflict},   // older serial
			{"POST", "?ID=l1", job.StateToken, state(1, "lin", ""), http.StatusConflict},   // same serial, other content
			{"POST", "?ID=l1", job.StateToken, state(2, "other", ""), http.StatusConflict}, // other lineage
			{"UNLOCK", "", job.StateToken, `{"ID":"l1"}`, http.StatusOK},
		})
		if rec := stateCall("GET", "", job.StateToken, ""); rec.Body.String() != v1 {
			t.Fatalf("read back state: %q", rec.Body.String())
		}
		v1s, _ = store.StateVersions(ctx, ws.ID, 10)
		if len(v1s) != 1 || v1s[0].Serial != 1 || v1s[0].Indexed {
			t.Fatalf("state versions: %+v", v1s)
		}
	})

	step(t, "a finished apply indexes its state and drops artifacts and intermediate versions", func(t *testing.T) {
		v2 := strings.Replace(state(2, "lin", `{"mode":"managed","type":"azurerm_storage_account","name":"this"}`), "{",
			`{"outputs":{"id":{"value":"abc","type":"string"},"key":{"value":"s3cret","type":"string","sensitive":true}},`, 1)
		saveState(t, v2)
		finish(t, `{"status":"succeeded","outputs":{"id":"abc"},
		"resources":[{"address":"azurerm_storage_account.this","type":"azurerm_storage_account","name":"this","provider":"registry.terraform.io/hashicorp/azurerm"}]}`)
		for _, name := range []string{runnerapi.ArtifactPlan, runnerapi.ArtifactLockfile} {
			if ok, _ := srv.App.Blobs.Exists(ctx, app.ArtifactKey(uuid.MustParse(runID), name)); ok {
				t.Fatalf("%s must be deleted once applied", name)
			}
		}
		if rec := stateCall("GET", "", job.StateToken, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("state access after the run: %d", rec.Code)
		}
		vs, _ := store.StateVersions(ctx, ws.ID, 10)
		if vs[0].Serial != 2 || !vs[0].Indexed || vs[0].Resources != 1 {
			t.Fatalf("the apply must index the current state version: %+v", vs)
		}
		// v1 was this run's intermediate snapshot: superseded by v2, so gone with its file
		if ok, _ := srv.App.Blobs.Exists(ctx, v1s[0].BlobKey); len(vs) != 1 || ok {
			t.Fatalf("intermediate state versions must be dropped when the run ends: %+v, file kept %v", vs, ok)
		}
		assertSealed(t, vs[0].BlobKey, "s3cret") // indexing read it back, so sealed state still works
	})

	// The stream replays everything, Terraform's colors included (the raw log drops them), then
	// ends with "done".
	t.Run("raw log and SSE stream replay the run's log", func(t *testing.T) {
		if rec := get(h, runURL+"/logs"); rec.Body.String() != "line one\nline two\n\nend\n" {
			t.Fatalf("raw log: %q", rec.Body.String())
		}
		start := time.Now()
		rec := get(h, runURL+"/logs/stream")
		if d := time.Since(start); d > time.Second { // an ended run is never signalled: its stream must not wait
			t.Errorf("the stream of an ended run took %s to end", d)
		}
		stream := rec.Body.String()
		if rec.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(stream, "data: line \x1b[1mtwo\x1b[0m\ndata: \ndata: end\n") ||
			!strings.HasSuffix(stream, "event: done\ndata: applied\n\n") {
			t.Fatalf("stream: %q", stream)
		}
		first := strings.TrimPrefix(strings.SplitN(stream, "\n", 2)[0], "id: ")
		req := httptest.NewRequest("GET", runURL+"/logs/stream", nil)
		req.Header.Set("Last-Event-ID", first)
		if resumed := do(h, req).Body.String(); strings.Contains(resumed, "line one") || !strings.Contains(resumed, "two") {
			t.Fatalf("resume: %q", resumed)
		}
		if rec := get(h, "/workspaces/"+name+"/runs/"+newID()+"/logs"); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown run: %d", rec.Code)
		}
	})

	t.Run("workspace page shows resources, outputs and templates", func(t *testing.T) {
		page := get(h, "/workspaces/"+name).Body.String() + get(h, "/workspaces/"+name+"?show=outputs").Body.String()
		for _, want := range []string{"<code>&#34;abc&#34;</code>", "<code>&#34;(sensitive)&#34;</code>", `<code title="azurerm_storage_account.this">azurerm_storage_account.this</code>`, "v2", "applied", "New run"} {
			if !strings.Contains(page, want) {
				t.Errorf("workspace page lacks %q", want)
			}
		}
		if tp := get(h, "/workspaces/"+name+"?show=templates").Body.String(); !strings.Contains(tp, "Templates 1<") || !strings.Contains(tp, `href="/templates/storage-account"`) {
			t.Error("templates tab lacks the template the workspace was created with")
		}
		if dp := get(h, "/workspaces/"+name+"/deploy").Body.String(); !strings.Contains(dp, `href="/workspaces/`+name+`/templates/new?template=storage-account"`) {
			t.Errorf("deploy page lacks the catalog: %s", dp)
		}
		if strings.Contains(page, "s3cret") {
			t.Fatal("sensitive output shown")
		}
		if !strings.Contains(get(h, "/workspaces").Body.String(), `href="/workspaces/`+name+`"`) {
			t.Fatal("workspace not listed")
		}
		if rec := get(h, "/resources"); rec.Code != http.StatusNotFound {
			t.Fatalf("the global resources page is gone: %d", rec.Code)
		}
	})

	step(t, "replace sends only addresses of the workspace's state to the runner", func(t *testing.T) {
		if rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}, "replace": {"-destroy"}}); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("replace of an unknown address: %d", rec.Code)
		}
		if rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"refresh"}, "replace": {"azurerm_storage_account.this"}}); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("replace in a refresh-only run: %d", rec.Code)
		}
		rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"apply"}, "replace": {"azurerm_storage_account.this"}})
		runURL = rec.Header().Get("Location")
		runID = strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
		claimJob(t, "plan")
		if len(job.Replace) != 1 || job.Replace[0] != "azurerm_storage_account.this" {
			t.Fatalf("replace job: %+v", job)
		}
		finish(t, `{"status":"succeeded"}`)
	})

	step(t, "a plan-only run with changes ends as planned", func(t *testing.T) {
		rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"plan"}})
		runID = strings.TrimPrefix(rec.Header().Get("Location"), "/workspaces/"+name+"/runs/")
		claimJob(t, "plan")
		finish(t, `{"status":"succeeded","plan":{"add":1,"change":0,"destroy":0,"changes":[{"address":"x.y","action":"create"}]}}`)
		if p, _, _ := store.GetRun(ctx, uuid.MustParse(runID)); p.Action != "plan" || p.Status != storage.Planned {
			t.Fatalf("plan only: %+v", p)
		}
	})

	step(t, "drift check is scheduled, never applied, kept out of the runs list", func(t *testing.T) {
		if rec := post(h, "/workspaces/"+name+"/settings/health", url.Values{"drift_detection": {"on"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("settings: %d", rec.Code)
		}
		if n, err := store.QueueAssessments(ctx, time.Hour); err != nil || n < 1 {
			t.Fatalf("queue assessments: %d %v", n, err)
		}
		a, err := store.LatestAssessment(ctx, ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		runID = a.ID.String()
		claimJob(t, "plan")
		finish(t, `{"status":"succeeded","plan":{"add":0,"change":1,"destroy":0,"changes":[{"address":"azurerm_storage_account.this","action":"update"}]}}`)
		if a, _, _ = store.GetRun(ctx, uuid.MustParse(runID)); a.Status != storage.Drifted {
			t.Fatalf("assessment with drift: %+v", a)
		}
		if n, _ := store.QueueAssessments(ctx, time.Hour); n != 0 {
			t.Fatalf("assessed again within the interval: %d", n)
		}
		if page := get(h, "/workspaces/"+name).Body.String(); !strings.Contains(page, "Drift detected") || !strings.Contains(get(h, "/workspaces").Body.String(), "drifted") {
			t.Fatalf("drift not shown: %s", page)
		}
		if runs, _ := store.Runs(ctx, ws.ID, 25); runs[0].Action == "assess" {
			t.Fatal("assessments must not be listed as runs")
		}
		post(h, "/workspaces/"+name+"/settings/health", url.Values{"drift_detection": {}})
	})

	t.Run("settings pages and workspace tabs render", func(t *testing.T) {
		if strings.Contains(get(h, "/workspaces/"+name).Body.String(), "Destroy…") || !strings.Contains(get(h, "/workspaces/"+name+"/settings/deletion").Body.String(), "Queue destroy plan") {
			t.Fatal("destroy belongs on the Destruction and deletion settings page, not the overview")
		}
		for _, page := range []string{"", "/health", "/lock", "/access", "/deletion"} { // the settings menu replaces the side nav; "</html>": rendered to the end
			if body := get(h, "/workspaces/"+name+"/settings"+page).Body.String(); !strings.Contains(body, `aria-label="Workspace settings"`) || !strings.Contains(body, "</html>") {
				t.Fatalf("settings%s: no settings menu or render error", page)
			}
		}
		for _, tab := range []string{"", "/runs", "/runs?filter=errored", "/states", "/variables", "?show=outputs"} { // each tab loads only its data
			if rec := get(h, "/workspaces/"+name+tab); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "</html>") {
				t.Fatalf("tab %q: %d or render error", tab, rec.Code)
			}
		}
		if !strings.Contains(get(h, "/workspaces/"+name+"/settings/health").Body.String(), "Drift detection:") ||
			!strings.Contains(get(h, "/workspaces/"+name+"/settings").Body.String(), "Save settings") {
			t.Fatal("settings General and Health pages")
		}
		if get(h, "/workspaces/"+name+"/settings/nope").Code != http.StatusNotFound {
			t.Fatal("unknown settings page must 404")
		}
	})

	destroyRun := func(t *testing.T) {
		t.Helper()
		rec := post(h, "/workspaces/"+name+"/runs", url.Values{"action": {"destroy"}})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("queue destroy: %d %s", rec.Code, rec.Body.String())
		}
		runURL = rec.Header().Get("Location")
		runID = strings.TrimPrefix(runURL, "/workspaces/"+name+"/runs/")
		claimJob(t, "plan")
		if job.Action != "destroy" {
			t.Fatalf("destroy job: %+v", job)
		}
		call("POST", runnerTok, "/jobs/"+runID+"/artifacts/"+runnerapi.ArtifactPlan, job.LeaseID, "DESTROYPLAN")
		finish(t, `{"status":"succeeded","plan":{"add":0,"change":0,"destroy":1,"changes":[{"address":"azurerm_storage_account.this","action":"delete"}]}}`)
	}
	step(t, "a destroy plan asks even with auto-apply; discarding deletes it", func(t *testing.T) {
		if rec := post(other, "/workspaces/"+name+"/runs", url.Values{"action": {"destroy"}}); rec.Code != http.StatusNotFound {
			t.Fatalf("destroy by a user without access: %d", rec.Code)
		}
		if rec := post(h, "/workspaces/"+name+"/settings", url.Values{"name": {name}, "execution_mode": {"remote"}, "auto_apply": {"on"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("settings: %d", rec.Code)
		}
		destroyRun(t)
		if !strings.Contains(get(h, runURL).Body.String(), "Confirm &amp; destroy") {
			t.Fatal("destroys must ask for confirmation even with auto-apply")
		}
		if rec := post(h, runURL+"/discard", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("discard: %d", rec.Code)
		}
		if ok, _ := srv.App.Blobs.Exists(ctx, app.ArtifactKey(uuid.MustParse(runID), runnerapi.ArtifactPlan)); ok {
			t.Fatal("a discarded plan must be deleted")
		}
	})

	step(t, "a confirmed destroy empties the state", func(t *testing.T) {
		destroyRun(t)
		post(h, runURL+"/confirm", nil)
		claimJob(t, "apply")
		saveState(t, state(3, "lin", ""))
		finish(t, `{"status":"succeeded","resources":[]}`)
		page := get(h, "/workspaces/"+name).Body.String()
		if strings.Contains(page, "azurerm_storage_account.this") || !strings.Contains(page, "destroyed") || !strings.Contains(page, "v3") {
			t.Fatalf("destroyed workspace still lists resources, or misses its state: %s", page)
		}
		if !strings.HasSuffix(get(h, runURL+"/logs/stream").Body.String(), "event: done\ndata: applied\n\n") {
			t.Fatal("log stream must end for finished runs")
		}
	})

	step(t, "rollback copies v2, its outputs and resource index, as v4", func(t *testing.T) {
		vs, _ := store.StateVersions(ctx, ws.ID, 10)
		rollback := func(id int64) int {
			return post(h, fmt.Sprintf("/workspaces/%s/state-versions/%d/rollback", name, id), nil).Code
		}
		if code := rollback(vs[0].ID); code != http.StatusConflict {
			t.Fatalf("rollback to the current version: %d", code)
		}
		if code := rollback(vs[0].ID + 1000); code != http.StatusNotFound {
			t.Fatalf("rollback to an unknown version: %d", code)
		}
		if code := post(other, fmt.Sprintf("/workspaces/%s/state-versions/%d/rollback", name, vs[1].ID), nil).Code; code != http.StatusNotFound {
			t.Fatalf("rollback by a user without access: %d", code)
		}
		// v2 vs the current v3 (destroyed): the rollback diff brings the output back
		if page := get(h, fmt.Sprintf("/workspaces/%s/states/%d", name, vs[1].ID)).Body.String(); !strings.Contains(page, "Rolling back to v2 changes") ||
			!strings.Contains(page, "output.id") || !strings.Contains(page, "Roll back</button>") {
			t.Fatalf("state version page: %s", page)
		}
		for _, p := range []string{fmt.Sprint(vs[0].ID + 1000), "x"} {
			if code := get(h, "/workspaces/"+name+"/states/"+p).Code; code != http.StatusNotFound {
				t.Fatalf("state version %s: %d", p, code)
			}
		}
		if code := get(other, fmt.Sprintf("/workspaces/%s/states/%d", name, vs[1].ID)).Code; code != http.StatusNotFound {
			t.Fatalf("state version page for a user without access: %d", code)
		}
		reader := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "flow-reader@example.com", false, "grp-flow-readers"))
		if err := store.SetGrant(ctx, ws.ID, "grp-flow-readers", "read"); err != nil {
			t.Fatal(err)
		}
		if code := get(reader, fmt.Sprintf("/workspaces/%s/states/%d", name, vs[1].ID)).Code; code != http.StatusForbidden {
			t.Fatalf("state values shown to read access: %d", code)
		}
		if code := rollback(vs[1].ID); code != http.StatusSeeOther {
			t.Fatalf("rollback: %d", code)
		}
		vs, _ = store.StateVersions(ctx, ws.ID, 10)
		if vs[0].Serial != 4 || !vs[0].Indexed || vs[0].Resources != 1 || vs[0].CopyOf != 2 {
			t.Fatalf("rolled back version: %+v", vs[0])
		}
		if out, _, _ := store.CurrentOutputs(ctx, ws.ID); out["id"] != "abc" {
			t.Fatalf("rolled back outputs: %v", out)
		}
		if page := get(h, "/workspaces/"+name+"/states").Body.String(); !strings.Contains(page, "rollback to v2") {
			t.Fatalf("version list must show the rollback: %s", page)
		}
		if b, _ := srv.App.ReadBlob(ctx, vs[0].BlobKey); !strings.Contains(string(b), `"serial": 4`) || !strings.Contains(string(b), "azurerm_storage_account") {
			t.Fatalf("rolled back state: %s", b)
		}
		if lock, _ := store.StateLockID(ctx, ws.ID); lock != "" {
			t.Fatal("rollback must release the state lock")
		}
	})
}

// A run whose log retention removed: the page, the raw log and the stream all say so, in one line.
func TestRemovedLog(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h := srv.Handler()
	ws, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "retained"})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := store.Pool.QueryRow(ctx, `WITH r AS (INSERT INTO runs (workspace_id, action, status, log_bytes, updated_at)
		VALUES ($1, 'apply', 'applied', 18, now() - interval '100 days') RETURNING id)
		INSERT INTO run_logs (run_id, chunk) SELECT id, 'secret plan output' FROM r RETURNING run_id::text`, ws.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	srv.App.RunLogRetention = 90 * 24 * time.Hour
	if _, err := srv.App.Retention(ctx); err != nil {
		t.Fatal(err)
	}
	const line = "The log was removed 100 days after the run ended (RUN_LOG_RETENTION_DAYS)."
	runURL := "/workspaces/retained/runs/" + id
	if page := get(h, runURL).Body.String(); !strings.Contains(page, line) || strings.Contains(page, "secret plan output") {
		t.Errorf("run page: %s", page)
	}
	if rec := get(h, runURL+"/logs"); rec.Code != http.StatusOK || rec.Body.String() != line+"\n" {
		t.Errorf("raw log: %d %q", rec.Code, rec.Body.String())
	}
	if s := get(h, runURL+"/logs/stream").Body.String(); s != "data: "+line+"\n\nevent: done\ndata: applied\n\n" {
		t.Errorf("stream: %q", s)
	}
}

// TestPlanDiffPage: a plan's attribute changes are on its run page for readers, like the log; a value holding a
// workspace secret is hidden by the server too, whatever the runner sent.
func TestPlanDiffPage(t *testing.T) {
	_, _, h, r := flowServer(t)
	ws := "pd-" + strings.ToLower(newID()[:8])
	run := greetingWorkspace(t, h, ws, false)
	if rec := post(h, "/workspaces/"+ws+"/variables", url.Values{"category": {"env"}, "key": {"TOKEN"}, "value": {"hunter2-secret"},
		"sensitive": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("variable: %d %s", rec.Code, rec.Body.String())
	}
	r.claim(t, run, "plan")
	r.finish(t, `{"status": "succeeded", "plan": {"add": 0, "change": 1, "destroy": 0, "changes": [{"address": "x.y", "action": "update"}]},
		"diff": [{"address": "x.y", "action": "update", "attrs": [
			{"path": "size", "before": "\"s\"", "after": "\"m\"", "has_before": true, "has_after": true},
			{"path": "cmd", "before": "\"echo hunter2-secret\"", "after": "\"x\"", "has_before": true, "has_after": true}]}]}`)
	page := get(h, "/workspaces/"+ws+"/runs/"+run).Body.String()
	if !strings.Contains(page, `<code>size</code>`) || !strings.Contains(page, `<code>&#34;m&#34;</code>`) || !strings.Contains(page, "(sensitive)") ||
		strings.Contains(page, "hunter2-secret") {
		t.Fatalf("run page:\n%s", page)
	}
}
