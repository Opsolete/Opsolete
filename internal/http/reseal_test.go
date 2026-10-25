package httpserver

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Values sealed before associated data (nonce + ciphertext, no AD) are re-sealed at startup and
// then open in place; nothing is lost.
func TestResealLegacyValues(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	key := vcs.Key("reseal-test")
	legacy := func(plain string) []byte {
		b, _ := aes.NewCipher(key)
		g, _ := cipher.NewGCM(b)
		nonce := make([]byte, g.NonceSize())
		rand.Read(nonce)
		return g.Seal(nonce, nonce, []byte(plain), nil)
	}
	name := "reseal-" + newID()[:8]
	if err := store.CreateVariableSet(ctx, name, "", false); err != nil {
		t.Fatal(err)
	}
	set, _ := store.GetVariableSet(ctx, name)
	o := storage.VarOwner{SetID: set.ID}
	if err := store.SetVariable(ctx, o, storage.Variable{Category: "env", Key: "OLD", Value: legacy("v1"), Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	connID, err := store.CreateVCSConnection(ctx, storage.VCSConnection{Name: name, Provider: vcs.GitHub, BaseURL: "https://api.github.com", Token: legacy("tok")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.DeleteVariableSet(ctx, set.ID); store.DeleteVCSConnection(ctx, connID) })
	if n, err := vcs.Reseal(ctx, store, key); err != nil || n < 2 {
		t.Fatalf("reseal: %d %v", n, err)
	}
	vars, _ := store.Variables(ctx, o)
	if got, err := vcs.Open(key, vars[0].Value, vcs.VariableAD(vars[0])); err != nil || got != "v1" {
		t.Fatalf("variable after reseal: %q %v", got, err)
	}
	c, _ := store.GetVCSConnection(ctx, connID)
	if conn, err := vcs.ConnFor(key, c); err != nil || conn.Token != "tok" {
		t.Fatalf("token after reseal: %q %v", conn.Token, err)
	}
	c.BaseURL = "https://evil.example.com" // a database write alone can't redirect the token
	if _, err := vcs.ConnFor(key, c); err == nil {
		t.Fatal("token opened for another endpoint")
	}
	if n, _ := vcs.Reseal(ctx, store, key); n != 0 {
		t.Fatalf("re-sealed twice: %d", n)
	}
}
