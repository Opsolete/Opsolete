package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// Locking: a lock (with its reason) keeps runs queued, except plans that are
// never applied; only its holder unlocks,
// anyone with operate access force-unlocks.
func TestWorkspaceLock(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	alice := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "alice@example.com", true))
	bob := newServerAs(t, "../../catalog", false, store, devAuth(t, store, "bob@example.com", true))
	ws, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "lk-" + strings.ToLower(newID()[:8])})
	if err != nil {
		t.Fatal(err)
	}
	u := "/workspaces/" + ws.Name

	if rec := post(alice, u+"/lock", url.Values{"action": {"lock"}, "reason": {strings.Repeat("x", 301)}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a reason over 300 characters: %d", rec.Code)
	}
	if rec := post(alice, u+"/lock", url.Values{"action": {"lock"}, "reason": {"migrating state"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("lock: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(bob, u+"/lock", url.Values{"action": {"lock"}}); rec.Code != http.StatusConflict {
		t.Fatalf("locking a locked workspace: %d", rec.Code)
	}
	if body := get(alice, u+"/settings/lock").Body.String(); !strings.Contains(body, "migrating state") || !strings.Contains(body, "Unlock workspace") {
		t.Fatal("the holder sees the reason and Unlock")
	}
	if body := get(bob, u+"/settings/lock").Body.String(); !strings.Contains(body, "Force unlock workspace") {
		t.Fatal("others see Force unlock")
	}

	// plans that are never applied run anyway; the runner plans them without the lock
	assess, err := store.CreateRun(ctx, ws.ID, "assess", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j, ok, err := store.ClaimJob(ctx, "r", time.Minute); err != nil || !ok || j.RunID != assess.ID {
		t.Fatalf("a drift check must run in a locked workspace: %+v %v %v", j, ok, err)
	}
	if _, err := store.CancelRun(ctx, assess.ID, "t", true); err != nil {
		t.Fatal(err)
	}
	if l, _ := store.GetStateLock(ctx, ws.ID); l == nil {
		t.Fatal("a drift check's end released the workspace lock")
	}
	run, err := store.CreateRun(ctx, ws.ID, "apply", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimJob(ctx, "r", time.Minute); err != nil || ok {
		t.Fatalf("a locked workspace's run was claimed: %v %v", ok, err)
	}
	post(bob, u+"/lock", url.Values{"action": {"unlock"}}) // not bob's lock: no effect
	if l, _ := store.GetStateLock(ctx, ws.ID); l == nil {
		t.Fatal("unlock released another user's lock")
	}
	post(bob, u+"/lock", url.Values{"action": {"force-unlock"}})
	if l, _ := store.GetStateLock(ctx, ws.ID); l != nil {
		t.Fatalf("force unlock: %+v", l)
	}
	if j, ok, err := store.ClaimJob(ctx, "r", time.Minute); err != nil || !ok || j.RunID != run.ID {
		t.Fatalf("the run must start once unlocked: %+v %v %v", j, ok, err)
	}
}
