// Package secrets seals values with SECRET_KEY (AES-256-GCM) and opens them with SECRET_KEY or
// SECRET_KEY_PREVIOUS, so a key rotation needs a restart, not downtime.
//
// Sealed format: "opsolete:1:" + key id (16 hex) + ":" + nonce (12 bytes) + ciphertext. The key id
// names the key that sealed it (see keyID); the associated data (ad) is not stored: the caller
// passes the same ad to Open, so a value copied to another row or file doesn't open there.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const prefix = "opsolete:1:"

// idLen is the key id's length in hex.
const idLen = 16

// maxPlain bounds a sealed value (and so the allocation in Seal): 64 MiB.
const maxPlain = 64 << 20

type key struct {
	id   string
	aead cipher.AEAD
}

// Keyring seals with the current key and opens with the current or the previous one.
type Keyring struct {
	current  key
	previous *key // nil without SECRET_KEY_PREVIOUS
}

// New builds a keyring from SECRET_KEY and SECRET_KEY_PREVIOUS ("" for none).
func New(current, previous string) (*Keyring, error) {
	if current == "" {
		return nil, errors.New("SECRET_KEY is empty")
	}
	if previous == current {
		return nil, errors.New("SECRET_KEY_PREVIOUS equals SECRET_KEY")
	}
	k := &Keyring{current: derive(current)}
	if previous != "" {
		p := derive(previous)
		k.previous = &p
	}
	return k, nil
}

// derive turns a secret into an AES-256 key and its key id: a hash of the key, which doesn't reveal it.
func derive(secret string) key {
	raw := sha256.Sum256([]byte("opsolete seal\n" + secret))
	id := sha256.Sum256(append([]byte("opsolete key id\n"), raw[:]...))
	b, _ := aes.NewCipher(raw[:]) // a 32-byte key never fails
	g, _ := cipher.NewGCM(b)
	return key{id: hex.EncodeToString(id[:idLen/2]), aead: g}
}

// Seal encrypts plain with the current key, bound to ad.
func (k *Keyring) Seal(plain, ad string) ([]byte, error) {
	g := k.current.aead
	if len(plain) > maxPlain {
		return nil, fmt.Errorf("can't seal: value larger than %d bytes", maxPlain)
	}
	out := make([]byte, 0, len(prefix)+idLen+1+g.NonceSize()+len(plain)+g.Overhead())
	out = append(append(append(out, prefix...), k.current.id...), ':')
	nonce := make([]byte, g.NonceSize())
	rand.Read(nonce)
	out = append(out, nonce...)
	return g.Seal(out, nonce, []byte(plain), []byte(ad)), nil
}

// Open decrypts a value sealed with the same ad by the current or the previous key.
func (k *Keyring) Open(sealed []byte, ad string) (string, error) {
	id, body, ok := split(sealed)
	if !ok {
		return "", errors.New("can't decrypt: not a sealed value")
	}
	var g cipher.AEAD
	switch {
	case id == k.current.id:
		g = k.current.aead
	case k.previous != nil && id == k.previous.id:
		g = k.previous.aead
	default:
		return "", fmt.Errorf("sealed with key %s, which is neither SECRET_KEY nor SECRET_KEY_PREVIOUS", id)
	}
	if len(body) < g.NonceSize() {
		return "", errors.New("can't decrypt: sealed value too short")
	}
	b, err := g.Open(nil, body[:g.NonceSize()], body[g.NonceSize():], []byte(ad))
	if err != nil {
		return "", errors.New("can't decrypt: the value was changed or moved")
	}
	return string(b), nil
}

// IsSealed tells whether b has the sealed format (by any key).
func IsSealed(b []byte) bool { _, _, ok := split(b); return ok }

// NeedsReseal tells whether sealed isn't sealed with the current key (rotate-key re-seals it).
func (k *Keyring) NeedsReseal(sealed []byte) bool {
	id, _, ok := split(sealed)
	return !ok || id != k.current.id
}

func split(sealed []byte) (id string, body []byte, ok bool) {
	rest, ok := bytes.CutPrefix(sealed, []byte(prefix))
	if !ok || len(rest) < idLen+1 || rest[idLen] != ':' {
		return "", nil, false
	}
	return string(rest[:idLen]), rest[idLen+1:], true
}
