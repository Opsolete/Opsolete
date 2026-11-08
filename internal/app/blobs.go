package app

import (
	"bytes"
	"context"
	"fmt"

	"gocloud.dev/blob"

	"github.com/opsolete/opsolete/internal/vcs"
)

// SealedPrefix marks state store files encrypted with SECRET_KEY; files without it are plaintext
// (written before SECRET_KEY was set) and are still read as is.
const SealedPrefix = "opsolete-sealed-v1\n"

// BlobAD binds a sealed file to its key: a state file copied over another workspace's doesn't open.
func BlobAD(key string) string { return "blob\x00" + key }

// WriteBlob stores a state file or run artifact, encrypted (AES-256-GCM) with SECRET_KEY:
// they hold secrets, so bucket read access alone must not expose them.
func (a *App) WriteBlob(ctx context.Context, key string, body []byte) error {
	sealed, err := vcs.Seal(a.SecretKey, string(body), BlobAD(key))
	if err != nil {
		return err
	}
	body = append([]byte(SealedPrefix), sealed...)
	return a.Blobs.WriteAll(ctx, key, body, &blob.WriterOptions{ContentType: "application/octet-stream"})
}

// ReadBlob reads a file written by WriteBlob, decrypting it if it was sealed.
func (a *App) ReadBlob(ctx context.Context, key string) ([]byte, error) {
	b, err := a.Blobs.ReadAll(ctx, key)
	if err != nil || !bytes.HasPrefix(b, []byte(SealedPrefix)) {
		return b, err
	}
	plain, err := vcs.Open(a.SecretKey, b[len(SealedPrefix):], BlobAD(key))
	if err != nil { // sealed before files were bound to their key
		plain, err = vcs.OpenLegacy(a.SecretKey, b[len(SealedPrefix):])
	}
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: SECRET_KEY changed?", key)
	}
	return []byte(plain), nil
}
