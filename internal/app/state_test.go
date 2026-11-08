package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/registry"
	"github.com/opsolete/opsolete/internal/storage"
)

// A run's state credential: only its own run's token, only while it works, writes only while
// applying, changes only its own workspace's state, reads another's only if shared and not from a
// pull request's plan.
func TestStateRunCredential(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	a.Registry = &registry.Registry{Key: []byte("state-test")}
	ws, run := configured(t, a)
	leased(t, a, run.ID, "l")
	token := a.StateToken(ws.ID, run.ID)
	open := func(state uuid.UUID, tok string) (StateRun, error) {
		return a.StateRun(ctx, state, ws.ID.String(), tok)
	}
	if _, err := open(ws.ID, a.StateToken(ws.ID, uuid.New())); !errors.Is(err, ErrForbidden) {
		t.Errorf("another run's token: %v", err)
	}
	if _, err := a.StateRun(ctx, ws.ID, "not-an-id", token); !errors.Is(err, ErrForbidden) {
		t.Errorf("a username that isn't an id: %v", err)
	}
	own, err := open(ws.ID, token)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := own.Read(ctx); err != nil || b != nil {
		t.Errorf("read own, no state yet: %q %v", b, err)
	}
	if _, ok, err := own.Lock(ctx, []byte(`{"ID":"l1"}`)); err != nil || !ok {
		t.Fatalf("lock own: %v %v", ok, err)
	}
	if err := own.Write(ctx, "l1", []byte(`{"serial":1,"lineage":"x"}`)); !errors.Is(err, ErrForbidden) {
		t.Errorf("a plan writes: %v", err)
	}
	if _, _, err := own.Lock(ctx, []byte(`{}`)); !isValidation(err) {
		t.Errorf("lock info without ID: %v", err)
	}

	other := emptyWorkspace(t, a, admin)
	foreign, err := open(other.ID, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Read(ctx); !errors.Is(err, ErrForbidden) {
		t.Errorf("read a state not shared: %v", err)
	}
	if err := a.ShareState(ctx, admin, other.Name, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Read(ctx); err != nil {
		t.Errorf("read a shared state: %v", err)
	}
	if _, _, err := foreign.Lock(ctx, []byte(`{"ID":"x"}`)); !errors.Is(err, ErrForbidden) {
		t.Errorf("lock another workspace's state: %v", err)
	}
	if err := foreign.Unlock(ctx, []byte(`{"ID":"x"}`)); !errors.Is(err, ErrForbidden) {
		t.Errorf("unlock another workspace's state: %v", err)
	}

	// a pull request's speculative plan: its own state, never a shared one
	if _, err := a.Store.Pool.Exec(ctx, `UPDATE runs SET action = 'plan', vcs_pull_request = 7 WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if foreign, err = open(other.ID, token); err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Read(ctx); !errors.Is(err, ErrForbidden) {
		t.Errorf("a pull request plan reads a shared state: %v", err)
	}
	if own, err = open(ws.ID, token); err != nil {
		t.Fatal(err)
	}
	if _, err := own.Read(ctx); err != nil {
		t.Errorf("a pull request plan reads its own state: %v", err)
	}

	// ended: the credential is gone
	if _, err := a.CancelRun(ctx, admin, ws.Name, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := open(ws.ID, token); !errors.Is(err, ErrForbidden) {
		t.Errorf("an ended run's token: %v", err)
	}
}

// saveVersions saves state files of lineage "lin" with these serials as the workspace's versions.
func saveVersions(t *testing.T, a *App, wsID uuid.UUID, serials ...int) {
	t.Helper()
	for _, n := range serials {
		body := fmt.Appendf(nil, `{"serial":%d,"lineage":"lin","outputs":{"n":{"value":%d}},"resources":[]}`, n, n)
		if err := a.saveState(context.Background(), wsID, body, func(v storage.StateVersion) error {
			return a.Store.AddStateVersion(context.Background(), wsID, uuid.Nil, "", v)
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func unlocked(t *testing.T, a *App, wsID uuid.UUID, when string) {
	t.Helper()
	if l, err := a.Store.GetStateLock(context.Background(), wsID); err != nil || l != nil {
		t.Errorf("%s: lock left behind: %+v %v", when, l, err)
	}
}

// A rollback refuses while a run is active or the state is locked, leaves no lock behind when it
// fails, and copies the version with a new serial.
func TestRollback(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	ws, run := configured(t, a) // a queued run
	saveVersions(t, a, ws.ID, 1, 2)
	vs, _ := a.Store.StateVersions(ctx, ws.ID, 2)
	v1, v2 := vs[1].ID, vs[0].ID
	st, err := a.State(ctx, admin, ws.Name, LevelOperate)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Rollback(ctx, v1); !errors.Is(err, storage.ErrRunActive) {
		t.Errorf("during a run: %v", err)
	}
	unlocked(t, a, ws.ID, "refused for a run")
	if _, err := a.CancelRun(ctx, admin, ws.Name, run.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := st.Lock(ctx, "busy"); err != nil {
		t.Fatal(err)
	}
	if err := st.Rollback(ctx, v1); !errors.Is(err, storage.ErrStateLocked) || !IsConflict(err) {
		t.Errorf("while locked: %v", err)
	}
	if l, _ := a.Store.GetStateLock(ctx, ws.ID); l == nil || l.Info != "busy" {
		t.Errorf("a refused rollback touched the user's lock: %+v", l)
	}
	if err := st.Unlock(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		version int64
		want    func(error) bool
	}{
		{v2 + 1000, func(err error) bool { return errors.Is(err, ErrNotFound) }},
		{v2, IsConflict}, // the current one
	} {
		if err := st.Rollback(ctx, c.version); !c.want(err) {
			t.Errorf("rollback to %d: %v", c.version, err)
		}
		unlocked(t, a, ws.ID, fmt.Sprint("failed rollback to ", c.version))
	}
	if _, err := a.State(ctx, user(t, a), ws.Name, LevelOperate); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger opens the state: %v", err)
	}

	if err := st.Rollback(ctx, v1); err != nil {
		t.Fatal(err)
	}
	unlocked(t, a, ws.ID, "rollback")
	vs, _ = a.Store.StateVersions(ctx, ws.ID, 1)
	if vs[0].Serial != 3 || vs[0].CopyOf != 1 {
		t.Fatalf("rolled back version: %+v", vs[0])
	}
	if out, _, _ := a.Store.CurrentOutputs(ctx, ws.ID); fmt.Sprint(out["n"]) != "1" {
		t.Fatalf("rolled back outputs: %v", out)
	}
}

// State values (diffs, files) are shown to operators only; readers see the version list.
func TestStateDiffAccess(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	group := uniq("grp-")
	reader := user(t, a, group)
	ws := emptyWorkspace(t, a, admin)
	if err := a.SetGrant(ctx, admin, ws.Name, group, "read"); err != nil {
		t.Fatal(err)
	}
	saveVersions(t, a, ws.ID, 1, 2)
	vs, _ := a.Store.StateVersions(ctx, ws.ID, 2)

	ro, err := a.State(ctx, reader, ws.Name, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := ro.Versions(ctx, 10); err != nil || len(list) != 2 {
		t.Errorf("a reader lists versions: %v %v", list, err)
	}
	if _, err := ro.Diff(ctx, vs[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader diffs: %v", err)
	}
	if _, err := ro.File(ctx, vs[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader downloads: %v", err)
	}
	if err := ro.Lock(ctx, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader locks: %v", err)
	}

	op, err := a.State(ctx, admin, ws.Name, LevelRead)
	if err != nil {
		t.Fatal(err)
	}
	d, err := op.Diff(ctx, vs[1].ID)
	if err != nil || d.Prev != nil || d.IsCurrent || len(d.Changes) != 1 || len(d.Rollback) != 1 || d.Rollback[0].Attrs[0].New != "1" {
		t.Fatalf("an operator diffs v1: %+v %v", d, err)
	}
	if d, err := op.Diff(ctx, vs[0].ID); err != nil || !d.IsCurrent || d.Prev == nil || d.Rollback != nil {
		t.Fatalf("an operator diffs the current version: %+v %v", d, err)
	}
	if _, err := op.Diff(ctx, vs[0].ID+1000); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version: %v", err)
	}
}

// TestReleaseOrphanedLocks (E5): the leader's job releases a run's lock once the run stopped
// working (here its runner vanished before acknowledging, so it was queued again with its
// Terraform's lock left behind) and an old rollback's, logs each and wakes claimers: the run
// waiting behind its own lost lock is claimed. CLI and UI locks stay.
func TestReleaseOrphanedLocks(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	n := listening(t, a, time.Hour)
	var logged bytes.Buffer
	a.Log = slog.New(slog.NewTextHandler(&logged, nil))
	lock := func(ws uuid.UUID, id string, run uuid.UUID, age string) {
		t.Helper()
		if _, ok, err := a.Store.LockState(ctx, ws, id, run, []byte(`{"ID":"`+id+`"}`)); err != nil || !ok {
			t.Fatalf("lock %s: %v %v", id, ok, err)
		}
		if _, err := a.Store.Pool.Exec(ctx, `UPDATE workspaces SET state_locked_at = now() - $2::interval WHERE id = $1`, ws, age); err != nil {
			t.Fatal(err)
		}
	}
	ws, run := configured(t, a)
	lock(ws.ID, "tf", run.ID, "0s")
	rollback, newRollback, cli := emptyWorkspace(t, a, admin), emptyWorkspace(t, a, admin), emptyWorkspace(t, a, admin)
	lock(rollback.ID, "rb1", uuid.Nil, "1 hour")
	lock(newRollback.ID, "rb2", uuid.Nil, "1 minute")
	lock(cli.ID, storage.CloudLockPrefix+"x", uuid.Nil, "1 day")
	if _, ok, err := a.Store.ClaimJob(ctx, "r", time.Minute); err != nil || ok {
		t.Fatalf("claimed behind a state lock: %v %v", ok, err)
	}

	jobs := n.Jobs()
	defer jobs.Close()
	a.ReleaseOrphanedLocks(ctx, 15*time.Minute)
	if !signalled(jobs, time.Second) {
		t.Fatal("releasing locks didn't wake claimers")
	}
	for w, want := range map[uuid.UUID]string{ws.ID: "", rollback.ID: "", newRollback.ID: "rb2", cli.ID: storage.CloudLockPrefix + "x"} {
		if got, _ := a.Store.StateLockID(ctx, w); got != want {
			t.Errorf("workspace %s: lock %q, want %q", w, got, want)
		}
	}
	for _, want := range []string{`msg="orphaned state lock released" workspace=` + ws.ID.String(), `lock="{\"ID\": \"tf\"`,
		`msg="orphaned state lock released" workspace=` + rollback.ID.String()} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, logged.String())
		}
	}
	if strings.Count(logged.String(), "orphaned state lock released") != 2 {
		t.Errorf("logged releases:\n%s", logged.String())
	}
	if j, ok, err := a.Store.ClaimJob(ctx, "r", time.Minute); err != nil || !ok || j.RunID != run.ID {
		t.Fatalf("the run behind the released lock: %+v %v %v", j, ok, err)
	}
	a.ReleaseOrphanedLocks(ctx, 15*time.Minute) // its new claim works with a lease: its next lock stays
	lock(ws.ID, "tf2", run.ID, "1 day")
	a.ReleaseOrphanedLocks(ctx, 15*time.Minute)
	if got, _ := a.Store.StateLockID(ctx, ws.ID); got != "tf2" {
		t.Fatalf("a working run's lock was released: %q", got)
	}
}
