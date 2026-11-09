package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/secrets"
)

// The signing key behind state credentials and signed URLs is stored sealed and never changes:
// converting 0.6.0's unsealed key keeps the key 0.6.0 derived, and a rotation (restart with both
// keys, rotate-key, drop the previous one) keeps it too, so credentials in flight stay valid.
func TestSigningKeyKeptAcrossRotation(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	const oldSecret, newSecret = "old-secret-key-0123456789abcdef0123", "new-secret-key-0123456789abcdef0123"
	stored, err := a.Store.SigningKey(ctx) // as 0.6.0 left it: random, unsealed
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte(oldSecret))
	m.Write(stored)
	want := m.Sum(nil) // what 0.6.0 signed with

	oldKeys := keys(oldSecret, "")
	if got, err := SigningKey(ctx, a.Store, oldKeys, oldSecret); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("upgrade from 0.6.0 must keep its key: %x %v", got, err)
	}
	if now, _ := a.Store.SigningKey(ctx); !secrets.IsSealed(now) || bytes.Contains(now, want) {
		t.Fatal("the signing key must be stored sealed")
	}
	if got, _ := SigningKey(ctx, a.Store, oldKeys, "ignored once sealed"); !bytes.Equal(got, want) {
		t.Fatal("a second start must open the same key")
	}

	// rotation: restart with both keys, rotate-key, then the new key alone
	both := keys(newSecret, oldSecret)
	if got, err := SigningKey(ctx, a.Store, both, newSecret); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("restart with the new key: %x %v", got, err)
	}
	if _, err := RotateKey(ctx, a.Store, nil, both, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if got, err := SigningKey(ctx, a.Store, keys(newSecret, ""), newSecret); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("after rotate-key, the new key alone: %x %v", got, err)
	}
	if _, err := SigningKey(ctx, a.Store, oldKeys, oldSecret); err == nil {
		t.Fatal("after rotate-key the old key alone must no longer open it")
	}
}

// An upgrade done while rotating (servers start with SECRET_KEY new and SECRET_KEY_PREVIOUS old)
// converts with the old key: the one 0.6.0 derived with.
func TestSigningKeyUpgradeWhileRotating(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	const oldSecret, newSecret = "old-secret-key-0123456789abcdef0123", "new-secret-key-0123456789abcdef0123"
	stored, _ := a.Store.SigningKey(ctx)
	m := hmac.New(sha256.New, []byte(oldSecret))
	m.Write(stored)
	if got, err := SigningKey(ctx, a.Store, keys(newSecret, oldSecret), oldSecret); err != nil || !bytes.Equal(got, m.Sum(nil)) {
		t.Fatalf("got %x %v", got, err)
	}
}

// Replicas starting together convert the key once and all get the same one.
func TestSigningKeyConcurrentStarts(t *testing.T) {
	a, ctx := testApp(t), context.Background()
	const secret = "the-secret-key-0123456789abcdef0123"
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	errs := make([]error, 8)
	for i := range got {
		wg.Go(func() { got[i], errs[i] = SigningKey(ctx, a.Store, keys(secret, ""), secret) })
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || len(got[i]) != sha256.Size || !bytes.Equal(got[i], got[0]) {
			t.Fatalf("start %d: %x %v (first %x)", i, got[i], errs[i], got[0])
		}
	}
}
