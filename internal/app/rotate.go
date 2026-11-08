package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"gocloud.dev/blob"
	"gocloud.dev/gcerrors"

	"github.com/opsolete/opsolete/internal/secrets"
	"github.com/opsolete/opsolete/internal/storage"
)

// RotateKey re-seals under the current key (SECRET_KEY) everything sealed with another one
// (SECRET_KEY_PREVIOUS): variables, VCS tokens, sensitive template inputs and the files in the
// state store (bucket; nil without deployments). Servers keep running: they open either key and
// seal with the current one. Values already under the current key are skipped, so it can run again
// after a failure. It refuses while a run is active, and fails if one started meanwhile: a run's
// saved configuration (runs.config_changes) holds sealed inputs it doesn't re-seal, which a failed
// run restores; run it again once that run has ended. Values that open with neither key were already
// unreadable: they are logged and left alone.
func RotateKey(ctx context.Context, st *storage.Store, bucket *blob.Bucket, keys *secrets.Keyring, log *slog.Logger) (int, error) {
	if active, err := st.AnyActiveRun(ctx); err != nil || active {
		return 0, cmp.Or(err, errors.New("a run is active: wait until it ends (or discard it), then rotate"))
	}
	n := 0
	// reseal returns sealed under the current key; nil if it needs nothing (already, or unreadable).
	reseal := func(what string, sealed []byte, ad string) ([]byte, error) {
		if !keys.NeedsReseal(sealed) {
			return nil, nil
		}
		plain, err := keys.Open(sealed, ad)
		if err != nil {
			log.Warn("can't be opened, left as it is", "value", what, "err", err)
			return nil, nil
		}
		n++
		return keys.Seal(plain, ad)
	}
	// stored counts a value that changed between reading and re-sealing it (the servers keep running).
	raced := 0
	stored := func(err error) error {
		if errors.Is(err, storage.ErrNotFound) {
			raced++
			return nil
		}
		return err
	}

	vars, err := st.AllVariables(ctx)
	if err != nil {
		return n, err
	}
	for _, v := range vars {
		if sealed, err := reseal("variable "+v.Key, v.Value, VariableAD(v)); err != nil {
			return n, err
		} else if sealed != nil {
			if err := stored(st.ResealVariable(ctx, v.ID, v.Value, sealed)); err != nil {
				return n, err
			}
		}
	}

	conns, err := st.VCSConnections(ctx)
	if err != nil {
		return n, err
	}
	for _, c := range conns {
		if sealed, err := reseal("VCS token "+c.Name, c.Token, TokenAD(c)); err != nil {
			return n, err
		} else if sealed != nil {
			if err := stored(st.ResealVCSToken(ctx, c.ID, c.Token, sealed)); err != nil {
				return n, err
			}
		}
	}

	uses, err := st.AllTemplateInputs(ctx)
	if err != nil {
		return n, err
	}
	for _, u := range uses {
		var inputs map[string]any
		if err := json.Unmarshal(u.Inputs, &inputs); err != nil {
			return n, fmt.Errorf("inputs of %s: %w", u.Name, err)
		}
		changed := false
		for k, v := range inputs {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			str, _ := m["sealed"].(string)
			old, err := base64.StdEncoding.DecodeString(str)
			if err != nil {
				return n, fmt.Errorf("input %s of %s: %w", k, u.Name, err)
			}
			sealed, err := reseal("input "+k+" of "+u.Name, old, InputAD(u.WorkspaceID, u.Name, k))
			if err != nil {
				return n, err
			}
			if sealed != nil {
				inputs[k], changed = map[string]any{"sealed": base64.StdEncoding.EncodeToString(sealed)}, true
			}
		}
		if changed {
			raw, err := json.Marshal(inputs)
			if err != nil {
				return n, err
			}
			if err := stored(st.ResealInputs(ctx, u.WorkspaceID, u.Name, u.Inputs, raw)); err != nil {
				return n, err
			}
		}
	}

	if bucket != nil {
		if err := resealBlobs(ctx, bucket, reseal); err != nil {
			return n, err
		}
	}
	if raced > 0 {
		return n, fmt.Errorf("%d values changed while being re-sealed: run rotate-key again", raced)
	}
	if active, err := st.AnyActiveRun(ctx); err != nil || active {
		return n, cmp.Or(err, errors.New("a run started during the rotation: run rotate-key again once it has ended"))
	}
	return n, nil
}

// resealBlobs re-seals every file in the state store. Files are written once under a new key, so a
// server writing meanwhile doesn't race with it; a file deleted meanwhile is skipped.
func resealBlobs(ctx context.Context, bucket *blob.Bucket, reseal func(string, []byte, string) ([]byte, error)) error {
	it := bucket.List(nil)
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if obj.IsDir {
			continue
		}
		b, err := bucket.ReadAll(ctx, obj.Key)
		if gcerrors.Code(err) == gcerrors.NotFound {
			continue
		} else if err != nil {
			return err
		}
		old, ok := bytes.CutPrefix(b, []byte(SealedPrefix))
		if !ok {
			return fmt.Errorf("file %s: not a sealed Opsolete file (is STATE_STORE shared with something else?)", obj.Key)
		}
		sealed, err := reseal("file "+obj.Key, old, BlobAD(obj.Key))
		if err != nil {
			return err
		}
		if sealed != nil {
			if err := bucket.WriteAll(ctx, obj.Key, append([]byte(SealedPrefix), sealed...),
				&blob.WriterOptions{ContentType: "application/octet-stream"}); err != nil {
				return err
			}
		}
	}
}
