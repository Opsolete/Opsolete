package app

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// rotate-key re-seals variables, VCS tokens, sensitive inputs and state store files from the old
// SECRET_KEY to the new one, refuses while a run is active, and a second run has nothing to do.
func TestRotateKey(t *testing.T) {
	a := testApp(t)
	store, ctx := a.Store, context.Background()
	store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'errored', lease_id = NULL, lease_until = NULL WHERE run_active(status)`)
	oldKey, newKey := vcs.Key("rotate-old-"+uniq("")), vcs.Key("rotate-new-"+uniq(""))
	seal := func(plain, ad string) []byte {
		b, err := vcs.Seal(oldKey, plain, ad)
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
	v.Value = seal("v1", vcs.VariableAD(v))
	if err := store.SetVariable(ctx, o, v); err != nil {
		t.Fatal(err)
	}
	conn := storage.VCSConnection{Name: name, Provider: vcs.GitHub, BaseURL: "https://api.github.com"}
	conn.Token = seal("tok", vcs.TokenAD(conn))
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
	t.Cleanup(func() {
		store.DeleteVariableSet(ctx, set.ID)
		store.DeleteVCSConnection(ctx, connID)
		store.Pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1::uuid`, ws.ID)
	})

	bucket := memblob.OpenBucket(nil)
	(&App{Blobs: bucket, SecretKey: oldKey}).WriteBlob(ctx, "opsolete/x/1.tfstate", []byte("state"))
	legacy, _ := vcs.Seal(oldKey, "legacy", "") // sealed before files were bound to their key
	legacy = legacy[len("opsolete-v2:"):]
	bucket.WriteAll(ctx, "opsolete/x/0.tfstate", append([]byte(SealedPrefix), legacy...), nil)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := RotateKey(ctx, store, bucket, oldKey, newKey, log); err == nil {
		t.Fatal("rotation while a run is active")
	}
	store.Pool.Exec(context.Background(), `UPDATE runs SET status = 'errored', lease_id = NULL, lease_until = NULL WHERE id = $1::uuid`, run.ID)
	if n, err := RotateKey(ctx, store, bucket, oldKey, newKey, log); err != nil || n != 5 {
		t.Fatalf("rotate: %d %v", n, err)
	}

	vars, _ := store.Variables(ctx, o)
	if got, err := vcs.Open(newKey, vars[0].Value, vcs.VariableAD(vars[0])); err != nil || got != "v1" {
		t.Fatalf("variable: %q %v", got, err)
	}
	c, _ := store.GetVCSConnection(ctx, connID)
	if cf, err := vcs.ConnFor(newKey, c); err != nil || cf.Token != "tok" {
		t.Fatalf("token: %v", err)
	}
	uses, _ := store.WorkspaceTemplates(ctx, ws.ID)
	rotated := &App{Blobs: bucket, SecretKey: newKey}
	if in, _, err := rotated.OpenInputs(uses[0].Inputs, ws.ID, "main"); err != nil || in["password"] != "pw" || in["plain"] != "x" {
		t.Fatalf("inputs: %v %v", in, err)
	}
	for key, want := range map[string]string{"opsolete/x/1.tfstate": "state", "opsolete/x/0.tfstate": "legacy"} {
		if b, err := rotated.ReadBlob(ctx, key); err != nil || string(b) != want {
			t.Fatalf("file %s: %q %v", key, b, err)
		}
	}
	if n, err := RotateKey(ctx, store, bucket, oldKey, newKey, log); err != nil || n != 0 {
		t.Fatalf("second rotation: %d %v", n, err)
	}
}
