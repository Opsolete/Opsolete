package app

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/storage"
)

// TestBlobEncryption: state store files are sealed with SECRET_KEY; a file without the sealed prefix is refused.
func TestBlobEncryption(t *testing.T) {
	ctx := context.Background()
	a := &App{Blobs: memblob.OpenBucket(nil), Secrets: keys("test-secret-key", "")}
	if err := a.WriteBlob(ctx, "a", []byte(`{"password":"hunter2"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := a.Blobs.ReadAll(ctx, "a"); bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("stored in plaintext: %s", raw)
	}
	if b, err := a.ReadBlob(ctx, "a"); err != nil || string(b) != `{"password":"hunter2"}` {
		t.Fatalf("read back: %s %v", b, err)
	}
	a.Blobs.WriteAll(ctx, "plain", []byte(`{"serial":1}`), nil)
	if b, err := a.ReadBlob(ctx, "plain"); err == nil {
		t.Fatalf("read a plaintext file: %s", b)
	}
	a.Blobs.Copy(ctx, "b", "a", nil)
	if _, err := a.ReadBlob(ctx, "b"); err == nil {
		t.Fatal("a file copied to another key must not open")
	}
	if _, err := (&App{Blobs: a.Blobs, Secrets: keys("other", "")}).ReadBlob(ctx, "a"); err == nil {
		t.Fatal("a wrong key must fail")
	}
}

// A sensitive variable's value copied into a non-sensitive row (which the UI shows) doesn't open there.
func TestVariableAD(t *testing.T) {
	k := keys("test-secret-key", "")
	v := storage.Variable{Category: "env", Key: "K", Sensitive: true, Owner: storage.VarOwner{SetID: uuid.New()}}
	sealed, _ := k.Seal("secret", VariableAD(v))
	v.Sensitive = false
	if _, err := k.Open(sealed, VariableAD(v)); err == nil {
		t.Fatal("sensitivity is not bound to the value")
	}
}
