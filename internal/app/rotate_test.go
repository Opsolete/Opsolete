package app

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Servers with both keys open values sealed with the old one; rotate-key re-seals variables, VCS
// tokens, sensitive inputs and state store files under the new one (refusing while a run is
// active), after which the new key alone opens everything and a second run has nothing to do.
func TestRotateKey(t *testing.T) {
	a := testApp(t)
	store, ctx := a.Store, context.Background()
	oldSecret, newSecret := "rotate-old-"+uniq(""), "rotate-new-"+uniq("")
	oldKey, both, newKey := keys(oldSecret, ""), keys(newSecret, oldSecret), keys(newSecret, "")
	seal := func(plain, ad string) []byte {
		b, err := oldKey.Seal(plain, ad)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	name := uniq("rotate-")

	if err := store.CreateVariableSet(ctx, name, "", false); err != nil {
		t.Fatal(err)
	}
	set, _ := store.GetVariableSet(ctx, name)
	o := storage.VarOwner{SetID: set.ID}
	v := storage.Variable{Owner: o, Category: "env", Key: "SECRET", Sensitive: true}
	v.Value = seal("v1", VariableAD(v))
	if err := store.SetVariable(ctx, o, v); err != nil {
		t.Fatal(err)
	}
	conn := storage.VCSConnection{Name: name, Provider: vcs.GitHub, BaseURL: "https://api.github.com"}
	conn.Token = seal("tok", TokenAD(conn))
	connID, err := store.CreateVCSConnection(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	wsID := uuid.New()
	input := map[string]any{"sealed": base64.StdEncoding.EncodeToString(seal(`"pw"`, InputAD(wsID, "main", "password")))}
	ws, run, err := store.CreateWorkspace(ctx, wsID, name, "", storage.WorkspaceTemplate{Name: "main", TemplateID: "t",
		ModuleSource: "git::https://x//m", ModuleRef: "v1", Inputs: map[string]any{"password": input, "plain": "x"}}, false, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}

	bucket := memblob.OpenBucket(nil)
	(&App{Blobs: bucket, Secrets: oldKey}).WriteBlob(ctx, "opsolete/x/1.tfstate", []byte("state"))
	rolled := &App{Blobs: bucket, Secrets: both} // restarted with SECRET_KEY=new, SECRET_KEY_PREVIOUS=old
	if b, err := rolled.ReadBlob(ctx, "opsolete/x/1.tfstate"); err != nil || string(b) != "state" {
		t.Fatalf("old file with both keys: %q %v", b, err)
	}
	rolled.WriteBlob(ctx, "opsolete/x/2.tfstate", []byte("new")) // written after the restart: already under the new key
	if got, err := both.Open(v.Value, VariableAD(v)); err != nil || got != "v1" {
		t.Fatalf("old variable with both keys: %q %v", got, err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := RotateKey(ctx, store, bucket, both, log); err == nil {
		t.Fatal("rotation while a run is active")
	}
	store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'errored', lease_id = NULL, lease_until = NULL WHERE id = $1::uuid`, run.ID)
	if n, err := RotateKey(ctx, store, bucket, both, log); err != nil || n != 4 {
		t.Fatalf("rotate: %d %v", n, err)
	}

	vars, _ := store.Variables(ctx, o)
	if got, err := newKey.Open(vars[0].Value, VariableAD(vars[0])); err != nil || got != "v1" {
		t.Fatalf("variable: %q %v", got, err)
	}
	c, _ := store.GetVCSConnection(ctx, connID)
	if cf, err := (&App{Secrets: newKey, VCS: vcs.NewClient(nil)}).conn(c); err != nil || cf.Token != "tok" {
		t.Fatalf("token: %v", err)
	}
	uses, _ := store.WorkspaceTemplates(ctx, ws.ID)
	rotated := &App{Blobs: bucket, Secrets: newKey}
	if in, _, err := rotated.OpenInputs(uses[0].Inputs, ws.ID, "main"); err != nil || in["password"] != "pw" || in["plain"] != "x" {
		t.Fatalf("inputs: %v %v", in, err)
	}
	for key, want := range map[string]string{"opsolete/x/1.tfstate": "state", "opsolete/x/2.tfstate": "new"} {
		if b, err := rotated.ReadBlob(ctx, key); err != nil || string(b) != want {
			t.Fatalf("file %s: %q %v", key, b, err)
		}
	}
	if n, err := RotateKey(ctx, store, bucket, both, log); err != nil || n != 0 {
		t.Fatalf("second rotation: %d %v", n, err)
	}
}

// onWarn is a log handler that calls f for each warning: a hook into RotateKey while it works.
type onWarn struct {
	slog.Handler
	f func()
}

func (h onWarn) Enabled(context.Context, slog.Level) bool { return true }

func (h onWarn) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.f()
	}
	return nil
}

// The servers keep running during rotate-key: a value changed between its reading and re-sealing
// keeps the server's write and fails the rotation, as does a run started meanwhile; both say to run
// it again. The hook is the warning about a value under neither key.
func TestRotateKeyRace(t *testing.T) {
	a := testApp(t)
	store, ctx := a.Store, context.Background()
	oldSecret, newSecret := "rotate-old-"+uniq(""), "rotate-new-"+uniq("")
	both, newKey := keys(newSecret, oldSecret), keys(newSecret, "")
	name := uniq("race-")
	if err := store.CreateVariableSet(ctx, name, "", false); err != nil {
		t.Fatal(err)
	}
	set, _ := store.GetVariableSet(ctx, name)
	o := storage.VarOwner{SetID: set.ID}
	put := func(key, plain string, k *secrets.Keyring) {
		t.Helper()
		v := storage.Variable{Owner: o, Category: "env", Key: key, Sensitive: true}
		var err error
		if v.Value, err = k.Seal(plain, VariableAD(v)); err != nil {
			t.Fatal(err)
		}
		if err := store.SetVariable(ctx, o, v); err != nil {
			t.Fatal(err)
		}
	}
	// read in insertion order (a fresh table's sequential scan): LOST's warning comes before GOOD is re-sealed
	put("LOST", "x", keys("rotate-lost-"+uniq(""), ""))
	put("GOOD", "v1", keys(oldSecret, ""))
	var hook func()
	log := slog.New(onWarn{slog.DiscardHandler, func() { hook() }})

	hook = func() { put("GOOD", "v2", both) } // a server saves GOOD meanwhile, under the new key
	if _, err := RotateKey(ctx, store, nil, both, log); err == nil || !strings.Contains(err.Error(), "1 values changed while being re-sealed: run rotate-key again") {
		t.Fatalf("a value changed meanwhile: %v", err)
	}
	vars, _ := store.Variables(ctx, o)
	for _, v := range vars {
		if got, err := newKey.Open(v.Value, VariableAD(v)); v.Key == "GOOD" && (err != nil || got != "v2") {
			t.Fatalf("the server's write must stay: %q %v", got, err)
		}
	}

	hook = func() { // a run starts meanwhile
		if _, _, err := store.CreateWorkspace(ctx, uuid.New(), uniq("race-"), "", storage.WorkspaceTemplate{Name: "main", TemplateID: "t",
			ModuleSource: "git::https://x//m", ModuleRef: "v1"}, false, uuid.Nil); err != nil {
			t.Error(err)
		}
		hook = func() {}
	}
	if _, err := RotateKey(ctx, store, nil, both, log); err == nil || !strings.Contains(err.Error(), "a run started during the rotation") {
		t.Fatalf("a run started meanwhile: %v", err)
	}
}
