package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestRunners: a runner is who its token says, not what it sends; a project's pool (its settings) sends its jobs
// to that pool's runners only; a deleted runner's token stops working.
func TestRunners(t *testing.T) {
	_, store, h, r := flowServer(t)
	ctx := context.Background()
	sfx := strings.ToLower(newID()[:8])

	page := post(h, "/organization/runners", url.Values{"name": {"prd-" + sfx}, "pool": {"prd"}})
	body := page.Body.String()
	i := strings.Index(body, "OPS_RUNNER_TOKEN=")
	if page.Code != http.StatusOK || i < 0 || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d\n%s", page.Code, body)
	}
	prdToken := strings.Fields(body[i+len("OPS_RUNNER_TOKEN="):])[0]
	prd := &fakeRunner{h: h, token: prdToken}

	proj := "pool-" + sfx
	if rec := post(h, "/projects", url.Values{"name": {proj}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("project: %d", rec.Code)
	}
	if !strings.Contains(get(h, "/projects/"+proj+"/settings").Body.String(), `<option value="prd">prd</option>`) {
		t.Fatal("the pool picker doesn't offer prd")
	}
	if rec := post(h, "/projects/"+proj+"/settings", url.Values{"name": {proj}, "runner_pool": {"prd"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("project pool: %d %s", rec.Code, rec.Body.String())
	}
	ws := "pw-" + sfx
	run := runID(t, post(h, "/templates/greeting", url.Values{fieldWorkspace: {ws}, fieldProject: {proj}, "greeting": {"hi"}, "name": {"x"}}))

	r.noJob(t, "a default-pool runner, the job's project in pool prd")
	prd.claim(t, run, "plan") // it sends "r1"; the server names it by its token
	if got := runStatus(t, store, run).Runner; got == nil || *got != "prd-"+sfx {
		t.Fatalf("claimed by %v, want prd-%s", got, sfx)
	}

	list, err := store.Runners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.Name == "prd-"+sfx {
			if rec := post(h, "/organization/runners/"+x.ID.String()+"/delete", nil); rec.Code != http.StatusSeeOther {
				t.Fatalf("delete: %d", rec.Code)
			}
		}
	}
	if c := prd.jobCall("POST", "heartbeat", ""); c != http.StatusUnauthorized {
		t.Fatalf("a deleted runner's heartbeat: %d", c)
	}
}
