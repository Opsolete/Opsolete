package app

import (
	"bytes"
	"context"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/opsolete/opsolete/internal/vcs"
)

// TestBlobEncryption: state store files are sealed with SECRET_KEY; plaintext ones from before still read.
func TestBlobEncryption(t *testing.T) {
	ctx := context.Background()
	a := &App{Blobs: memblob.OpenBucket(nil), SecretKey: vcs.Key("test-secret-key")}
	if err := a.WriteBlob(ctx, "a", []byte(`{"password":"hunter2"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := a.Blobs.ReadAll(ctx, "a"); bytes.Contains(raw, []byte("hunter2")) {
		t.Fatalf("stored in plaintext: %s", raw)
	}
	if b, err := a.ReadBlob(ctx, "a"); err != nil || string(b) != `{"password":"hunter2"}` {
		t.Fatalf("read back: %s %v", b, err)
	}
	a.Blobs.WriteAll(ctx, "legacy", []byte(`{"serial":1}`), nil)
	if b, err := a.ReadBlob(ctx, "legacy"); err != nil || string(b) != `{"serial":1}` {
		t.Fatalf("legacy plaintext: %s %v", b, err)
	}
	if _, err := (&App{Blobs: a.Blobs, SecretKey: vcs.Key("other")}).ReadBlob(ctx, "a"); err == nil {
		t.Fatal("a wrong key must fail")
	}
}
