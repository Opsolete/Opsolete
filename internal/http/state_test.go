package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// Remote state sharing: a run of another workspace may read (never write or lock) a state that is
// shared with it, all workspaces or by name.
func TestRemoteStateSharing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	app := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h := app.Handler()
	suffix := strings.ToLower(newID()[:8])
	src, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "rs-src-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	cons, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "rs-cons-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, cons.ID, "apply", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// as if a runner claimed it: the consumer's run is working, so its state credentials are valid
	if _, err := store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'planning', lease_id = 'l', lease_until = now() + interval '1 hour' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	state := func(method, token string) int {
		req := httptest.NewRequest(method, runnerapi.StatePrefix+"/"+src.ID.String(), strings.NewReader(`{"ID":"x"}`))
		req.SetBasicAuth(cons.ID.String(), token)
		return do(h, req).Code
	}
	share := func(mode string, consumers ...string) {
		t.Helper()
		if rec := post(h, "/workspaces/"+src.Name+"/settings/remote-state", url.Values{"remote_state": {mode}, "remote_state_consumers": consumers}); rec.Code != http.StatusSeeOther {
			t.Fatalf("share %s: %d %s", mode, rec.Code, rec.Body.String())
		}
	}
	token := app.App.StateToken(cons.ID, run.ID)
	if c := state("GET", token); c != http.StatusForbidden {
		t.Fatalf("not shared by default: %d", c)
	}
	share("specific", cons.ID.String(), "not-a-uuid", src.ID.String()) // unknown ids and itself are ignored
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("shared with the consumer: %d", c)
	}
	if c := state("GET", "forged"); c != http.StatusForbidden {
		t.Fatalf("forged consumer credentials: %d", c)
	}
	if c := state("LOCK", token); c != http.StatusForbidden {
		t.Fatalf("a consumer must not lock: %d", c)
	}
	if body := get(h, "/workspaces/"+src.Name+"/settings").Body.String(); !strings.Contains(body, `value="`+cons.ID.String()+`" checked>`) {
		t.Fatal("General settings must show the consumer selected")
	}
	share("all")
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("shared with all: %d", c)
	}
	share("specific")
	if c := state("GET", token); c != http.StatusForbidden {
		t.Fatalf("sharing turned off: %d", c)
	}
	// the other settings forms leave sharing alone
	share("all")
	post(h, "/workspaces/"+src.Name+"/settings/health", nil)
	post(h, "/workspaces/"+src.Name+"/settings", url.Values{"name": {src.Name}, "execution_mode": {"remote"}})
	if c := state("GET", token); c != http.StatusNoContent {
		t.Fatalf("General or Health save changed sharing: %d", c)
	}
	// a pull request's speculative plan runs unreviewed code: no shared state, even when shared with all
	if _, err := store.Pool.Exec(context.Background(), `UPDATE runs SET action = 'plan', vcs_pull_request = 7 WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if c := state("GET", token); c != http.StatusForbidden {
		t.Fatalf("pull request plan read shared state: %d", c)
	}
}
