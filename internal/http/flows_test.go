package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Flow tests (docs/flows.md): each follows one flow through the real HTTP stack, with fakeRunner
// making the runner's calls by hand.

// fakeRunner plays one runner against h: the runner API and Terraform's http state backend.
type fakeRunner struct {
	h   http.Handler
	job runnerapi.Job // the last claimed job
}

// call sends a runner API request with the runner token and the job's lease.
func (r *fakeRunner) call(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, runnerapi.Prefix+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testRunnerToken)
	req.Header.Set(runnerapi.LeaseHeader, r.job.LeaseID)
	return do(r.h, req)
}

// jobCall sends what ("heartbeat", "logs", "finish", "artifacts/tfplan") for the job and returns the status.
func (r *fakeRunner) jobCall(method, what, body string) int {
	return r.call(method, "/jobs/"+r.job.RunID+"/"+what, body).Code
}

// claim claims and acknowledges the next job, which must be phase of run runID.
func (r *fakeRunner) claim(t *testing.T, runID, phase string) runnerapi.Job {
	t.Helper()
	rec := r.call("POST", "/jobs/claim", `{"runner":"r1"}`)
	var j runnerapi.Job
	if json.Unmarshal(rec.Body.Bytes(), &j); rec.Code != http.StatusOK || j.RunID != runID || j.Phase != phase {
		t.Fatalf("claim %s of %s: %d %s", phase, runID, rec.Code, rec.Body.String())
	}
	r.job = j
	if c := r.jobCall("POST", "ack", ""); c != http.StatusNoContent {
		t.Fatalf("ack: %d", c)
	}
	return j
}

// noJob: nothing is claimable.
func (r *fakeRunner) noJob(t *testing.T, why string) {
	t.Helper()
	if rec := r.call("POST", "/jobs/claim", `{"runner":"r2"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("%s: claim got %d %s", why, rec.Code, rec.Body.String())
	}
}

// finish reports the job's result, which must be accepted.
func (r *fakeRunner) finish(t *testing.T, body string) {
	t.Helper()
	if c := r.jobCall("POST", "finish", body); c != http.StatusNoContent {
		t.Fatalf("finish %s: %d", body, c)
	}
}

// state calls the state backend with the job's state credential.
func (r *fakeRunner) state(method, query, body string) int {
	req := httptest.NewRequest(method, runnerapi.StatePrefix+"/"+r.job.WorkspaceID+query, strings.NewReader(body))
	req.SetBasicAuth(r.job.WorkspaceID, r.job.StateToken)
	return do(r.h, req).Code
}

// saveState writes body under a lock of its own, as Terraform's apply does.
func (r *fakeRunner) saveState(t *testing.T, body string) {
	t.Helper()
	for _, c := range [][3]string{{"LOCK", "", `{"ID":"s1"}`}, {"POST", "?ID=s1", body}, {"UNLOCK", "", `{"ID":"s1"}`}} {
		if code := r.state(c[0], c[1], c[2]); code != http.StatusOK {
			t.Fatalf("save state %s: %d", c[0], code)
		}
	}
}

// runID is the run a POST that queued one redirected to.
func runID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	loc := rec.Header().Get("Location")
	i := strings.Index(loc, "/runs/")
	if rec.Code != http.StatusSeeOther || i < 0 {
		t.Fatalf("no run queued: %d %q %s", rec.Code, loc, rec.Body.String())
	}
	return loc[i+len("/runs/"):]
}

// runStatus is run id's status as stored.
func runStatus(t *testing.T, store *storage.Store, id string) storage.Run {
	t.Helper()
	r, _, err := store.GetRun(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// pageHas fails unless the page at path shows every want.
func pageHas(t *testing.T, h http.Handler, path string, want ...string) {
	t.Helper()
	page := get(h, path).Body.String()
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("%s lacks %q", path, w)
		}
	}
}

const (
	planChanges = `{"status":"succeeded","plan":{"add":1,"change":0,"destroy":0,"changes":[{"address":"module.m.terraform_data.greeting","action":"create"}]}}`
	planNone    = `{"status":"succeeded"}`
)

// stateV is a state file of serial n with one resource.
func stateV(n int) string {
	return fmt.Sprintf(`{"version":4,"serial":%d,"lineage":"lin","resources":[{"mode":"managed","type":"terraform_data","name":"greeting"}]}`, n)
}

// flowServer: an admin's handler on a new database, the catalog's templates.
func flowServer(t *testing.T) (*Server, *storage.Store, http.Handler, *fakeRunner) {
	store := testStore(t)
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, Registry: true})
	h := srv.Handler()
	return srv, store, h, &fakeRunner{h: h}
}

// greetingWorkspace creates workspace name from the catalog's greeting template and returns its first run.
func greetingWorkspace(t *testing.T, h http.Handler, name string, autoApply bool) string {
	t.Helper()
	form := url.Values{fieldWorkspace: {name}, "greeting": {"hello"}, "name": {"world"}}
	if autoApply {
		form.Set(fieldAutoApply, "on")
	}
	return runID(t, post(h, "/templates/greeting", form))
}

// TestRefreshOnlyRun (C5): a refresh-only run plans, always asks (auto-apply on), and its confirmed
// apply updates the state only; discarded, nothing changes.
func TestRefreshOnlyRun(t *testing.T) {
	srv, store, h, r := flowServer(t)
	ctx := context.Background()
	ws := "rf-" + strings.ToLower(newID()[:8])
	first := greetingWorkspace(t, h, ws, true)
	r.claim(t, first, "plan")
	r.finish(t, planNone)
	w, _ := store.GetWorkspace(ctx, ws)

	var run string
	step(t, "a refresh-only plan with changes waits for confirmation even with auto-apply", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"refresh"}}))
		if j := r.claim(t, run, "plan"); j.Action != "refresh" {
			t.Fatalf("refresh job: %+v", j)
		}
		if c := r.jobCall("POST", "artifacts/"+runnerapi.ArtifactPlan, "REFRESHPLAN"); c != http.StatusNoContent {
			t.Fatalf("upload plan: %d", c)
		}
		r.finish(t, `{"status":"succeeded","plan":{"add":0,"change":1,"destroy":0,"changes":[{"address":"module.m.terraform_data.greeting","action":"update"}]}}`)
		if s := runStatus(t, store, run).Status; s != storage.NeedsConfirmation {
			t.Fatalf("refresh-only plan: %s", s)
		}
		r.noJob(t, "a refresh plan waiting for confirmation")
		pageHas(t, h, "/workspaces/"+ws+"/runs/"+run, "Refresh state", "needs confirmation", "Confirm &amp; apply", "Discard", `<span class="change update">~1</span>`)
	})
	step(t, "confirmed, its apply writes the refreshed state", func(t *testing.T) {
		if rec := post(h, "/workspaces/"+ws+"/runs/"+run+"/confirm", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("confirm: %d", rec.Code)
		}
		if j := r.claim(t, run, "apply"); j.Action != "refresh" {
			t.Fatalf("refresh apply job: %+v", j)
		}
		if rec := r.call("GET", "/jobs/"+run+"/artifacts/"+runnerapi.ArtifactPlan, ""); rec.Body.String() != "REFRESHPLAN" {
			t.Fatalf("the apply gets the refresh plan: %d %q", rec.Code, rec.Body.String())
		}
		r.saveState(t, strings.Replace(stateV(1), "{", `{"outputs":{"message":{"value":"hello, world!","type":"string"}},`, 1))
		r.finish(t, `{"status":"succeeded","outputs":{"message":"hello, world!"},"resources":[{"address":"module.m.terraform_data.greeting","type":"terraform_data","name":"greeting","provider":"terraform.io/builtin/terraform"}]}`)
		if s := runStatus(t, store, run).Status; s != storage.Applied {
			t.Fatalf("refresh run: %s", s)
		}
		vs, _ := store.StateVersions(ctx, w.ID, 10)
		if len(vs) != 1 || vs[0].Serial != 1 || !vs[0].Indexed || vs[0].Resources != 1 {
			t.Fatalf("state after the refresh: %+v", vs)
		}
		pageHas(t, h, "/workspaces/"+ws+"?show=outputs", "hello, world!")
	})
	step(t, "a discarded refresh-only plan changes nothing", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"refresh"}}))
		r.claim(t, run, "plan")
		r.jobCall("POST", "artifacts/"+runnerapi.ArtifactPlan, "REFRESHPLAN")
		r.finish(t, `{"status":"succeeded","plan":{"add":0,"change":1,"destroy":0}}`)
		if rec := post(h, "/workspaces/"+ws+"/runs/"+run+"/discard", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("discard: %d", rec.Code)
		}
		if s := runStatus(t, store, run).Status; s != storage.Discarded {
			t.Fatalf("discarded refresh: %s", s)
		}
		if ok, _ := srv.App.Blobs.Exists(ctx, app.ArtifactKey(uuid.MustParse(run), runnerapi.ArtifactPlan)); ok {
			t.Fatal("a discarded refresh plan must be deleted")
		}
		if vs, _ := store.StateVersions(ctx, w.ID, 10); len(vs) != 1 {
			t.Fatalf("a discarded refresh wrote state: %+v", vs)
		}
		r.noJob(t, "a discarded refresh")
	})
	step(t, "a refresh-only plan without changes ends as no changes", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"refresh"}}))
		r.claim(t, run, "plan")
		r.finish(t, planNone)
		if s := runStatus(t, store, run).Status; s != storage.NoChanges {
			t.Fatalf("refresh without changes: %s", s)
		}
	})
}

// versionedTemplates writes template "cfg" at module version ref into a new directory: two
// servers on one database with different directories are the catalog before and after a release.
func versionedTemplates(t *testing.T, ref string) string {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/cfg.json", []byte(`{"id":"cfg","name":"Cfg","module":{"source":"git::https://example.com/mods//cfg","ref":"`+ref+`"},
		"inputs":[{"name":"greeting","type":"string","default":"hello"},{"name":"name","type":"string","default":"one","key":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestAutoApply (C2): with auto-apply on, an apply plan with changes is applied without
// confirmation, one without changes ends; destroys and rollout plans still ask.
func TestAutoApply(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin := devAuth(t, store, "admin@example.com", true)
	srv := newApp(t, versionedTemplates(t, "v1"), store, admin, Features{Deployments: true})
	h := srv.Handler()
	r := &fakeRunner{h: h}
	ws := "aa-" + strings.ToLower(newID()[:8])
	run := runID(t, post(h, "/templates/cfg", url.Values{fieldWorkspace: {ws}, fieldAutoApply: {"on"}, "greeting": {"hello"}, "name": {"one"}}))
	w, _ := store.GetWorkspace(ctx, ws)

	step(t, "an apply plan with changes is applied without confirmation", func(t *testing.T) {
		r.claim(t, run, "plan")
		r.jobCall("POST", "artifacts/"+runnerapi.ArtifactPlan, "AUTOPLAN")
		r.finish(t, planChanges)
		if s := runStatus(t, store, run).Status; s != storage.ApplyQueued {
			t.Fatalf("auto-apply plan: %s", s)
		}
		pageHas(t, h, "/workspaces/"+ws+"/runs/"+run, "auto-apply: waiting for a runner")
		r.claim(t, run, "apply")
		if rec := r.call("GET", "/jobs/"+run+"/artifacts/"+runnerapi.ArtifactPlan, ""); rec.Body.String() != "AUTOPLAN" {
			t.Fatalf("the apply gets the plan: %d %q", rec.Code, rec.Body.String())
		}
		r.saveState(t, stateV(1))
		r.finish(t, `{"status":"succeeded","resources":[{"address":"module.one.terraform_data.greeting","type":"terraform_data","name":"greeting","provider":"p"}]}`)
		if got := runStatus(t, store, run); got.Status != storage.Applied || got.ConfirmedBy != "" {
			t.Fatalf("auto-applied run: %+v", got)
		}
		if vs, _ := store.StateVersions(ctx, w.ID, 1); len(vs) != 1 || !vs[0].Indexed {
			t.Fatalf("auto-applied state: %+v", vs)
		}
	})
	step(t, "an apply plan without changes ends as no changes", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}}))
		r.claim(t, run, "plan")
		r.finish(t, planNone)
		if s := runStatus(t, store, run).Status; s != storage.NoChanges {
			t.Fatalf("plan without changes: %s", s)
		}
		r.noJob(t, "nothing to apply")
	})
	step(t, "a plan-only run is never applied", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"plan"}}))
		r.claim(t, run, "plan")
		r.finish(t, planChanges)
		if s := runStatus(t, store, run).Status; s != storage.Planned {
			t.Fatalf("plan only: %s", s)
		}
	})
	step(t, "a destroy plan waits for confirmation", func(t *testing.T) {
		run = runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"destroy"}}))
		r.claim(t, run, "plan")
		r.finish(t, `{"status":"succeeded","plan":{"add":0,"change":0,"destroy":1}}`)
		if s := runStatus(t, store, run).Status; s != storage.NeedsConfirmation {
			t.Fatalf("destroy plan: %s", s)
		}
		post(h, "/workspaces/"+ws+"/runs/"+run+"/discard", nil)
	})
	step(t, "a rollout's plan waits for confirmation", func(t *testing.T) {
		h2 := newApp(t, versionedTemplates(t, "v2"), store, admin, Features{Deployments: true}).Handler()
		rec := post(h2, "/templates/cfg/rollout", url.Values{"source": {"git::https://example.com/mods//cfg"}, "ref": {"v2"}})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("start rollout: %d %s", rec.Code, rec.Body.String())
		}
		runs, _ := store.Runs(ctx, w.ID, 1)
		if len(runs) == 0 || runs[0].Status != storage.PlanQueued {
			t.Fatalf("the rollout queued no plan: %+v", runs)
		}
		run = runs[0].ID.String()
		if j := r.claim(t, run, "plan"); len(j.Modules) != 1 || !strings.HasSuffix(j.Modules[0].Source, "ref=v2") {
			t.Fatalf("rollout job: %+v", j)
		}
		r.finish(t, planChanges)
		if s := runStatus(t, store, run).Status; s != storage.NeedsConfirmation {
			t.Fatalf("rollout plan with auto-apply: %s", s)
		}
	})
}

// TestCancelRun (C9, C10, C11): a queued run is canceled at once; a running one when its runner
// stops Terraform, keeping the state it saved; force cancel cuts the runner off.
func TestCancelRun(t *testing.T) {
	srv, store, h, r := flowServer(t)
	ctx := context.Background()
	ws := "cn-" + strings.ToLower(newID()[:8])
	r.claim(t, greetingWorkspace(t, h, ws, false), "plan")
	r.finish(t, planNone)
	w, _ := store.GetWorkspace(ctx, ws)
	runURL := func(id string) string { return "/workspaces/" + ws + "/runs/" + id }
	lockID := func() string { id, _ := store.StateLockID(ctx, w.ID); return id }
	// planned starts an apply run and leaves it confirmed or waiting.
	planned := func(t *testing.T, confirm bool) string {
		t.Helper()
		id := runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}}))
		r.claim(t, id, "plan")
		r.jobCall("POST", "artifacts/"+runnerapi.ArtifactPlan, "PLAN")
		r.finish(t, planChanges)
		if confirm {
			post(h, runURL(id)+"/confirm", nil)
		}
		return id
	}

	step(t, "a queued run is canceled at once; the workspace lock and configuration stay, artifacts go", func(t *testing.T) {
		id := planned(t, false)
		if rec := post(h, "/workspaces/"+ws+"/lock", url.Values{"action": {"lock"}, "reason": {"maintenance"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("lock: %d %s", rec.Code, rec.Body.String())
		}
		lock := lockID()
		post(h, runURL(id)+"/confirm", nil) // apply_queued, waiting behind the lock
		r.noJob(t, "a locked workspace")
		before, _ := store.WorkspaceTemplates(ctx, w.ID)
		if rec := post(h, runURL(id)+"/cancel", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("cancel: %d", rec.Code)
		}
		if s := runStatus(t, store, id).Status; s != storage.Canceled {
			t.Fatalf("queued run after cancel: %s", s)
		}
		if lockID() != lock {
			t.Fatal("canceling a queued run released the workspace's lock")
		}
		if after, _ := store.WorkspaceTemplates(ctx, w.ID); len(after) != len(before) || after[0].Inputs["greeting"] != before[0].Inputs["greeting"] {
			t.Fatalf("configuration changed: %+v", after)
		}
		if ok, _ := srv.App.Blobs.Exists(ctx, app.ArtifactKey(uuid.MustParse(id), runnerapi.ArtifactPlan)); ok {
			t.Fatal("a canceled run's plan must be deleted")
		}
		pageHas(t, h, runURL(id), "canceled by admin@example.com")
		if rec := post(h, runURL(id)+"/cancel", nil); rec.Code != http.StatusConflict {
			t.Fatalf("cancel of an ended run: %d", rec.Code)
		}
		post(h, "/workspaces/"+ws+"/lock", url.Values{"action": {"unlock"}})
		if lockID() != "" {
			t.Fatal("unlock")
		}
	})
	step(t, "a running apply stops when its runner hears the cancel, keeping the state it saved", func(t *testing.T) {
		id := planned(t, true)
		r.claim(t, id, "apply")
		if c := r.state("LOCK", "", `{"ID":"tf1"}`); c != http.StatusOK {
			t.Fatalf("lock: %d", c)
		}
		if rec := post(h, runURL(id)+"/cancel", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("cancel: %d", rec.Code)
		}
		if s := runStatus(t, store, id).Status; s != storage.Applying {
			t.Fatalf("a running run ends only when its runner stops: %s", s)
		}
		pageHas(t, h, runURL(id), "Cancel requested…", "Force cancel", "cancel requested by admin@example.com")
		if c := r.jobCall("POST", "heartbeat", ""); c != runnerapi.StatusCancel {
			t.Fatalf("heartbeat after cancel: %d", c)
		}
		if c := r.jobCall("POST", "logs", "Interrupt received.\n"); c != runnerapi.StatusCancel {
			t.Fatalf("log after cancel: %d", c)
		}
		// Terraform, interrupted, saves what it created so far under its lock
		if c := r.state("POST", "?ID=tf1", stateV(1)); c != http.StatusOK {
			t.Fatalf("state write during a cancel: %d", c)
		}
		r.finish(t, `{"status":"canceled","message":"canceled by a user: Terraform was interrupted and saved its state"}`)
		if s := runStatus(t, store, id).Status; s != storage.Canceled {
			t.Fatalf("canceled run: %s", s)
		}
		if lockID() != "" {
			t.Fatal("the canceled run's state lock must be released")
		}
		vs, _ := store.StateVersions(ctx, w.ID, 10)
		if len(vs) != 1 || vs[0].Serial != 1 {
			t.Fatalf("the state saved during the cancel must stay current: %+v", vs)
		}
		if b, err := srv.App.ReadBlob(ctx, vs[0].BlobKey); err != nil || !strings.Contains(string(b), "terraform_data") {
			t.Fatalf("saved state: %q %v", b, err)
		}
		pageHas(t, h, runURL(id), "Terraform was interrupted and saved its state", `class="status canceled"`)
		if c := r.jobCall("POST", "logs", "late\n"); c != http.StatusConflict {
			t.Fatalf("log after the run ended: %d", c)
		}
	})
	step(t, "force cancel ends a running run at once and fences its runner off", func(t *testing.T) {
		id := planned(t, true)
		r.claim(t, id, "apply")
		if c := r.state("LOCK", "", `{"ID":"tf2"}`); c != http.StatusOK {
			t.Fatalf("lock: %d", c)
		}
		post(h, runURL(id)+"/cancel", nil)
		if rec := post(h, runURL(id)+"/cancel", url.Values{"force": {"1"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("force cancel: %d", rec.Code)
		}
		if s := runStatus(t, store, id).Status; s != storage.Canceled {
			t.Fatalf("force-canceled run: %s", s)
		}
		if lockID() != "" {
			t.Fatal("force cancel must release the run's state lock")
		}
		for what, c := range map[string]int{"heartbeat": r.jobCall("POST", "heartbeat", ""), "log": r.jobCall("POST", "logs", "x\n"),
			"finish": r.jobCall("POST", "finish", planNone)} {
			if c != http.StatusConflict {
				t.Errorf("%s of a force-canceled run: %d, want 409", what, c)
			}
		}
		if c := r.state("POST", "?ID=tf2", stateV(2)); c != http.StatusForbidden {
			t.Fatalf("state write of a force-canceled run: %d", c)
		}
		if vs, _ := store.StateVersions(ctx, w.ID, 10); len(vs) != 1 || vs[0].Serial != 1 {
			t.Fatalf("a fenced runner's state was saved: %+v", vs)
		}
		pageHas(t, h, runURL(id), "force-canceled by admin@example.com")
	})
}

// TestConfigRestore (D4): a run that changed a template's version or inputs, or added one, puts the
// previous configuration back when it ends before its apply; once its apply started it keeps it.
func TestConfigRestore(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	admin := devAuth(t, store, "admin@example.com", true)
	f := Features{Deployments: true}
	h1 := newApp(t, versionedTemplates(t, "v1"), store, admin, f).Handler()
	h := newApp(t, versionedTemplates(t, "v2"), store, admin, f).Handler() // the catalog after v2's release
	r := &fakeRunner{h: h}
	ws := "cr-" + strings.ToLower(newID()[:8])
	r.claim(t, runID(t, post(h1, "/templates/cfg", url.Values{fieldWorkspace: {ws}, "greeting": {"hello"}, "name": {"one"}})), "plan")
	r.finish(t, planNone)
	w, _ := store.GetWorkspace(ctx, ws)
	added, _ := store.WorkspaceTemplates(ctx, w.ID)
	module := added[0].Name
	templatesTab := "/workspaces/" + ws + "?show=templates"
	// current fails unless the workspace's templates are exactly want (name → "ref greeting").
	current := func(t *testing.T, want map[string]string) {
		t.Helper()
		got, _ := store.WorkspaceTemplates(ctx, w.ID)
		m := map[string]string{}
		for _, a := range got {
			m[a.Name] = a.ModuleRef + " " + a.Inputs["greeting"].(string)
		}
		if len(m) != len(want) {
			t.Fatalf("templates: %v, want %v", m, want)
		}
		for k, v := range want {
			if m[k] != v {
				t.Fatalf("templates: %v, want %v", m, want)
			}
		}
	}
	configure := func(t *testing.T) string {
		t.Helper()
		id := runID(t, post(h, "/workspaces/"+ws+"/templates/"+module, url.Values{"greeting": {"hey"}, "name": {"one"}}))
		current(t, map[string]string{module: "v2 hey"})
		return id
	}
	pageHas(t, h, templatesTab, `<span class="tag">v1</span>`, "update: v2")

	step(t, "a discarded plan of the new version puts the old version and inputs back", func(t *testing.T) {
		id := configure(t)
		if j := r.claim(t, id, "plan"); !strings.HasSuffix(j.Modules[0].Source, "ref=v2") || j.Modules[0].Inputs["greeting"] != "hey" {
			t.Fatalf("plan of the new configuration: %+v", j.Modules)
		}
		r.finish(t, planChanges)
		post(h, "/workspaces/"+ws+"/runs/"+id+"/discard", nil)
		current(t, map[string]string{module: "v1 hello"})
		pageHas(t, h, templatesTab, `<span class="tag">v1</span>`, "update: v2", "hello")
	})
	step(t, "a failed plan puts it back", func(t *testing.T) {
		id := configure(t)
		r.claim(t, id, "plan")
		r.finish(t, `{"status":"failed","message":"terraform plan failed"}`)
		if s := runStatus(t, store, id).Status; s != storage.Errored {
			t.Fatalf("failed plan: %s", s)
		}
		current(t, map[string]string{module: "v1 hello"})
	})
	step(t, "a canceled queued plan puts it back", func(t *testing.T) {
		id := configure(t)
		post(h, "/workspaces/"+ws+"/runs/"+id+"/cancel", nil)
		current(t, map[string]string{module: "v1 hello"})
	})
	step(t, "an added template whose plan is discarded is gone", func(t *testing.T) {
		id := runID(t, post(h, "/workspaces/"+ws+"/templates/new?template=cfg", url.Values{"greeting": {"hi"}, "name": {"two"}}))
		if got, _ := store.WorkspaceTemplates(ctx, w.ID); len(got) != 2 {
			t.Fatalf("added: %+v", got)
		}
		r.claim(t, id, "plan")
		r.finish(t, planChanges)
		post(h, "/workspaces/"+ws+"/runs/"+id+"/discard", nil)
		current(t, map[string]string{module: "v1 hello"})
		if page := get(h, templatesTab).Body.String(); strings.Contains(page, "two_") {
			t.Fatal("the discarded template is still listed")
		}
	})
	step(t, "a run whose apply started keeps the new configuration, even if the apply fails", func(t *testing.T) {
		id := configure(t)
		r.claim(t, id, "plan")
		r.finish(t, planChanges)
		post(h, "/workspaces/"+ws+"/runs/"+id+"/confirm", nil)
		r.claim(t, id, "apply")
		r.finish(t, `{"status":"failed","message":"apply failed halfway"}`)
		current(t, map[string]string{module: "v2 hey"})
		if page := get(h, templatesTab).Body.String(); !strings.Contains(page, `<span class="tag">v2</span>`) || strings.Contains(page, "update: v2") {
			t.Fatalf("templates tab after a started apply: %s", page)
		}
	})
}

// TestLeaseLoss (Q4): a runner that stops heartbeating loses its run: the reaper errors it, releases
// its state lock and drops its artifacts; the runner's late calls and state writes are refused.
func TestLeaseLoss(t *testing.T) {
	srv, store, h, r := flowServer(t)
	ctx := context.Background()
	ws := "ll-" + strings.ToLower(newID()[:8])
	id := greetingWorkspace(t, h, ws, false)
	r.claim(t, id, "plan")
	r.jobCall("POST", "artifacts/"+runnerapi.ArtifactPlan, "PLAN")
	r.finish(t, planChanges)
	post(h, "/workspaces/"+ws+"/runs/"+id+"/confirm", nil)
	r.claim(t, id, "apply")
	if c := r.state("LOCK", "", `{"ID":"tf"}`); c != http.StatusOK {
		t.Fatalf("lock: %d", c)
	}
	w, _ := store.GetWorkspace(ctx, ws)

	// the runner went silent for longer than its lease
	if _, err := store.Pool.Exec(ctx, `UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	srv.App.ReapLeases(ctx)
	if s := runStatus(t, store, id).Status; s != storage.Errored {
		t.Fatalf("run of a lost runner: %s", s)
	}
	if lock, _ := store.StateLockID(ctx, w.ID); lock != "" {
		t.Fatalf("the lost run's state lock must be released: %q", lock)
	}
	if ok, _ := srv.App.Blobs.Exists(ctx, app.ArtifactKey(uuid.MustParse(id), runnerapi.ArtifactPlan)); ok {
		t.Fatal("the lost run's plan must be deleted")
	}
	pageHas(t, h, "/workspaces/"+ws+"/runs/"+id, "runner stopped responding (lease expired)", `class="status errored"`)
	for what, c := range map[string]int{"heartbeat": r.jobCall("POST", "heartbeat", ""), "log": r.jobCall("POST", "logs", "late\n"),
		"finish": r.jobCall("POST", "finish", planNone), "plan download": r.jobCall("GET", "artifacts/"+runnerapi.ArtifactPlan, "")} {
		if c != http.StatusConflict {
			t.Errorf("%s after the lease was lost: %d, want 409", what, c)
		}
	}
	if c := r.state("POST", "?ID=tf", stateV(1)); c != http.StatusForbidden {
		t.Fatalf("state write after the lease was lost: %d", c)
	}
	if vs, _ := store.StateVersions(ctx, w.ID, 10); len(vs) != 0 {
		t.Fatalf("a lost runner's state was saved: %+v", vs)
	}
	// the workspace is free again
	r.claim(t, runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}})), "plan")
}

// TestJobVariables (G3, G4): the claimed job carries the workspace's variables over its attached sets,
// over its project's sets, over global sets; a VCS-linked workspace gets only the sets attached to it,
// and a changed link or turning on pull request plans detaches those. Sensitive values reach the job
// (and its Secrets, to mask) but no page.
func TestJobVariables(t *testing.T) {
	store := testStore(t)
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/repos":
			w.Write([]byte(`[{"full_name":"acme/infra"}]`))
		case "/repos/acme/infra/branches/main":
			w.Write([]byte(`{"commit":{"sha":"sha1"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true, VCS: true})
	srv.App.VCS = vcs.NewClient(gh.Client())
	h := srv.Handler()
	r := &fakeRunner{h: h}
	id := strings.ToLower(newID()[:8])
	ws, prj := "jv-"+id, "jv-prj-"+id
	setG, setP, setW := "jv-global-"+id, "jv-project-"+id, "jv-ws-"+id
	mustPost := func(t *testing.T, path string, form url.Values) {
		t.Helper()
		if rec := post(h, path, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s %v: %d %s", path, form, rec.Code, rec.Body.String())
		}
	}
	env := func(k, v string) url.Values { return url.Values{"category": {"env"}, "key": {k}, "value": {v}} }
	// job runs a plan and returns its job; the run ends without changes.
	job := func(t *testing.T) runnerapi.Job {
		t.Helper()
		j := r.claim(t, runID(t, post(h, "/workspaces/"+ws+"/runs", url.Values{"action": {"apply"}})), "plan")
		r.finish(t, planNone)
		return j
	}
	// has fails unless the job's env is exactly want (keys K1…K4, SEC, WSSEC: "" absent).
	has := func(t *testing.T, j runnerapi.Job, want map[string]string) {
		t.Helper()
		for k, v := range want {
			if got, ok := j.Env[k]; got != v || ok != (v != "") {
				t.Errorf("job env %s = %q (set %v), want %q", k, got, ok, v)
			}
		}
	}

	step(t, "workspace variables win over attached, project and global sets", func(t *testing.T) {
		mustPost(t, "/projects", url.Values{"name": {prj}})
		r.claim(t, runID(t, post(h, "/templates/greeting", url.Values{fieldWorkspace: {ws}, fieldProject: {prj}, "greeting": {"hello"}, "name": {"world"}})), "plan")
		r.finish(t, planNone)
		mustPost(t, "/variable-sets", url.Values{"name": {setG}, "global": {"on"}})
		mustPost(t, "/variable-sets", url.Values{"name": {setP}, "project": {prj}})
		mustPost(t, "/variable-sets", url.Values{"name": {setW}, "workspace": {ws}})
		for _, k := range []string{"K1", "K2", "K3", "K4"} {
			mustPost(t, "/variable-sets/"+setG+"/variables", env(k, "global"))
		}
		for _, k := range []string{"K1", "K2", "K3"} {
			mustPost(t, "/variable-sets/"+setP+"/variables", env(k, "project"))
		}
		for _, k := range []string{"K1", "K2"} {
			mustPost(t, "/variable-sets/"+setW+"/variables", env(k, "wsset"))
		}
		sec := env("SEC", "s3cret-set")
		sec.Set("sensitive", "on")
		mustPost(t, "/variable-sets/"+setW+"/variables", sec)
		mustPost(t, "/variable-sets/"+setG+"/variables", url.Values{"category": {"terraform"}, "key": {"region"}, "value": {"global"}})
		mustPost(t, "/variable-sets/"+setW+"/variables", url.Values{"category": {"terraform"}, "key": {"region"}, "value": {"wsset"}})
		mustPost(t, "/workspaces/"+ws+"/variables", env("K1", "ws"))
		wsSec := env("WSSEC", "s3cret-ws")
		wsSec.Set("sensitive", "on")
		mustPost(t, "/workspaces/"+ws+"/variables", wsSec)

		j := job(t)
		has(t, j, map[string]string{"K1": "ws", "K2": "wsset", "K3": "project", "K4": "global", "SEC": "s3cret-set", "WSSEC": "s3cret-ws"})
		if j.Vars["region"] != "wsset" {
			t.Errorf("job vars: %v", j.Vars)
		}
		if !slices.Contains(j.Secrets, "s3cret-set") || !slices.Contains(j.Secrets, "s3cret-ws") {
			t.Errorf("sensitive values must be in the job's Secrets, to mask: %v", j.Secrets)
		}
		for _, p := range []string{"/workspaces/" + ws + "/variables", "/variable-sets/" + setW, "/variable-sets/" + setG, "/variable-sets"} {
			if page := get(h, p).Body.String(); strings.Contains(page, "s3cret") {
				t.Errorf("%s shows a sensitive value", p)
			}
		}
		pageHas(t, h, "/workspaces/"+ws+"/variables", setG, setP, setW)
	})
	link := url.Values{"vcs_connection": nil, "vcs_repo": {"acme/infra"}, "vcs_branch": {"main"}}
	step(t, "a linked workspace gets only the sets attached to it, and linking detaches them", func(t *testing.T) {
		rec := post(h, "/vcs", url.Values{"name": {"gh-" + id}, "provider": {"github"}, "base_url": {gh.URL}, "token": {"pat"}})
		conn := strings.TrimSuffix(strings.TrimPrefix(rec.Header().Get("Location"), "/vcs/new?connection="), "&return=")
		mustPost(t, "/vcs/"+conn+"/settings", url.Values{"workspaces": {"1"}, "workspace_scope": {"all"}})
		link.Set("vcs_connection", conn)
		mustPost(t, "/workspaces/"+ws+"/vcs", link)
		if page := get(h, "/workspaces/"+ws+"/variables").Body.String(); strings.Contains(page, setW) || strings.Contains(page, setP) || strings.Contains(page, setG) {
			t.Fatal("a linked workspace's page must not list sets that no longer apply")
		}
		has(t, job(t), map[string]string{"K1": "ws", "K2": "", "K3": "", "K4": "", "SEC": "", "WSSEC": "s3cret-ws"})
		mustPost(t, "/variable-sets/"+setW+"/workspaces", url.Values{"workspace": {ws}}) // an admin vouches for the new link
		has(t, job(t), map[string]string{"K1": "ws", "K2": "wsset", "K3": "", "K4": "", "SEC": "s3cret-set"})
		mustPost(t, "/workspaces/"+ws+"/vcs", link) // saved unchanged: the sets stay
		has(t, job(t), map[string]string{"K2": "wsset"})
	})
	step(t, "turning on pull request plans detaches the workspace's sets", func(t *testing.T) {
		link.Set("vcs_pull_requests", "1")
		mustPost(t, "/workspaces/"+ws+"/vcs", link)
		has(t, job(t), map[string]string{"K1": "ws", "K2": "", "K3": "", "K4": "", "SEC": ""})
		if page := get(h, "/workspaces/"+ws+"/variables").Body.String(); strings.Contains(page, setW) {
			t.Fatal("a detached set is still listed")
		}
	})
}
