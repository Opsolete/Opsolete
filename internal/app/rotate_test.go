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
