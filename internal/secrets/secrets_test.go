package secrets

import (
	"bytes"
	"strings"
	"testing"
)

func keyring(t *testing.T, cur, prev string) *Keyring {
	t.Helper()
	k, err := New(cur, prev)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealOpen(t *testing.T) {
	k := keyring(t, "new-key", "")
	sealed, err := k.Seal("tok", "here")
	if err != nil || bytes.Contains(sealed, []byte("tok")) {
		t.Fatal("not sealed", err)
	}
	if !bytes.HasPrefix(sealed, []byte("opsolete:1:"+k.current.id+":")) || len(k.current.id) != 16 {
		t.Fatalf("format: %q", sealed)
	}
	if got, err := k.Open(sealed, "here"); err != nil || got != "tok" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := k.Open(sealed, "elsewhere"); err == nil || !strings.Contains(err.Error(), "can't decrypt") {
		t.Fatalf("opened with another ad: %v", err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := k.Open(tampered, "here"); err == nil || !strings.Contains(err.Error(), "can't decrypt") {
		t.Fatalf("opened tampered: %v", err)
	}
	if k.NeedsReseal(sealed) {
		t.Fatal("sealed with the current key needs no reseal")
	}
}

func TestRotation(t *testing.T) {
	old := keyring(t, "old-key", "")
	sealed, _ := old.Seal("v", "ad")
	both := keyring(t, "new-key", "old-key")
	if got, err := both.Open(sealed, "ad"); err != nil || got != "v" {
		t.Fatalf("previous key: %q %v", got, err)
	}
	if !both.NeedsReseal(sealed) || !both.NeedsReseal([]byte("garbage")) {
		t.Fatal("NeedsReseal")
	}
	resealed, _ := both.Seal("v", "ad")
	if both.NeedsReseal(resealed) {
		t.Fatal("Seal uses the current key")
	}
	_, err := keyring(t, "new-key", "").Open(sealed, "ad")
	if err == nil || err.Error() != "sealed with key "+old.current.id+", which is neither SECRET_KEY nor SECRET_KEY_PREVIOUS" {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := New("same", "same"); err == nil {
		t.Fatal("previous equal to current")
	}
	if strings.Contains(old.current.id, "old-key") {
		t.Fatal("key id reveals the key")
	}
}
