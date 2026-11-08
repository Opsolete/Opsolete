package app

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"gocloud.dev/blob"

	"github.com/opsolete/opsolete/internal/storage"
)

// Everything secret is sealed with App.Secrets (internal/secrets) together with associated data
// (AD) naming where it is stored: a value copied to another row, workspace or file doesn't open
// there. Each AD starts with the kind of value and joins its parts with NUL:
//   - VariableAD: owner (workspace or set), category, name and sensitivity, so a sensitive value
//     copied into a non-sensitive row (which the UI shows) doesn't open;
//   - TokenAD: the connection's provider, endpoint and name, so pointing base_url elsewhere in the
//     database doesn't send the token there;
//   - InputAD: a sensitive template input's workspace, module and name;
//   - BlobAD: a state store file's key, so a state file copied over another workspace's doesn't open.

// VariableAD binds a variable's value to its owner, name and sensitivity.
func VariableAD(v storage.Variable) string {
	return fmt.Sprintf("variable\x00%s\x00%s\x00%s\x00%s\x00%t", adID(v.Owner.WorkspaceID), adID(v.Owner.SetID), v.Category, v.Key, v.Sensitive)
}

// adID is an owner id in associated data: "" for none.
func adID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// TokenAD binds a VCS token to its connection's endpoint.
func TokenAD(c storage.VCSConnection) string {
	return "vcs token\x00" + c.Provider + "\x00" + c.BaseURL + "\x00" + c.Name
}

// InputAD binds a sealed input to its workspace, its module and its name.
func InputAD(workspaceID uuid.UUID, module, name string) string {
	return "template input\x00" + workspaceID.String() + "\x00" + module + "\x00" + name
}

// BlobAD binds a sealed file to its key.
func BlobAD(key string) string { return "blob\x00" + key }

// OpenToken opens a stored VCS connection's token.
func (a *App) OpenToken(c storage.VCSConnection) (string, error) {
	return a.Secrets.Open(c.Token, TokenAD(c))
}

// SealedPrefix starts every state store file; a file without it isn't Opsolete's.
const SealedPrefix = "opsolete-sealed-v1\n"

// WriteBlob stores a state file or run artifact, sealed: they hold secrets, so bucket read access
// alone must not expose them.
func (a *App) WriteBlob(ctx context.Context, key string, body []byte) error {
	sealed, err := a.Secrets.Seal(string(body), BlobAD(key))
	if err != nil {
		return err
	}
	body = append([]byte(SealedPrefix), sealed...)
	return a.Blobs.WriteAll(ctx, key, body, &blob.WriterOptions{ContentType: "application/octet-stream"})
}

// ReadBlob reads and opens a file written by WriteBlob.
func (a *App) ReadBlob(ctx context.Context, key string) ([]byte, error) {
	b, err := a.Blobs.ReadAll(ctx, key)
	if err != nil {
		return nil, err
	}
	sealed, ok := bytes.CutPrefix(b, []byte(SealedPrefix))
	if !ok {
		return nil, fmt.Errorf("decrypt %s: not a sealed Opsolete file", key)
	}
	plain, err := a.Secrets.Open(sealed, BlobAD(key))
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", key, err)
	}
	return []byte(plain), nil
}
