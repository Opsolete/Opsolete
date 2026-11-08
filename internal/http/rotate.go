package httpserver

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

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/vcs"
)

// RotateKey re-seals under newKey everything sealed with oldKey: variables, VCS tokens, sensitive
// template inputs and the files in the state store (bucket; nil without deployments). Values that
// already open with newKey are skipped, so it can run again after a failure. It refuses while a run
// is active: a run's saved configuration holds sealed inputs too. The server must be stopped.
// Values that open with neither key were already unreadable: they are logged and left alone.
func RotateKey(ctx context.Context, st *storage.Store, bucket *blob.Bucket, oldKey, newKey []byte, log *slog.Logger) (int, error) {
	if active, err := st.AnyActiveRun(ctx); err != nil || active {
		return 0, cmp.Or(err, errors.New("a run is active: wait until it ends (or discard it), then rotate"))
	}
	n := 0
	// reseal returns sealed under newKey; nil if it needs nothing (already newKey, or unreadable).
	// legacy also tries the format without ad: files sealed before they were bound to their key.
	reseal := func(what string, sealed []byte, ad string, legacy bool) ([]byte, error) {
		if _, err := vcs.Open(newKey, sealed, ad); err == nil {
			return nil, nil
		}
		plain, err := vcs.Open(oldKey, sealed, ad)
		if err != nil && legacy {
			plain, err = vcs.OpenLegacy(oldKey, sealed)
		}
		if err != nil {
			log.Warn("opens with neither key, left as it is", "value", what)
			return nil, nil
		}
		n++
		return vcs.Seal(newKey, plain, ad)
	}

	vars, err := st.AllVariables(ctx)
	if err != nil {
		return n, err
	}
	for _, v := range vars {
		if sealed, err := reseal("variable "+v.Key, v.Value, vcs.VariableAD(v), false); err != nil {
			return n, err
		} else if sealed != nil {
			if err := st.ResealVariable(ctx, v.ID, v.Value, sealed); err != nil {
				return n, err
			}
		}
	}

	conns, err := st.VCSConnections(ctx)
	if err != nil {
		return n, err
	}
	for _, c := range conns {
		if sealed, err := reseal("VCS token "+c.Name, c.Token, vcs.TokenAD(c), false); err != nil {
			return n, err
		} else if sealed != nil {
			if err := st.ResealVCSToken(ctx, c.ID, c.Token, sealed); err != nil {
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
			sealed, err := reseal("input "+k+" of "+u.Name, old, inputAD(u.WorkspaceID, u.Name, k), false)
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
			if err := st.ResealInputs(ctx, u.WorkspaceID, u.Name, u.Inputs, raw); err != nil {
				return n, err
			}
		}
	}

	if bucket == nil {
		return n, nil
	}
	it := bucket.List(nil)
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if obj.IsDir {
			continue
		}
		b, err := bucket.ReadAll(ctx, obj.Key)
		if err != nil {
			return n, err
		}
		if !bytes.HasPrefix(b, []byte(sealedPrefix)) {
			continue // plaintext, from before files were sealed
		}
		sealed, err := reseal("file "+obj.Key, b[len(sealedPrefix):], blobAD(obj.Key), true)
		if err != nil {
			return n, err
		}
		if sealed != nil {
			if err := bucket.WriteAll(ctx, obj.Key, append([]byte(sealedPrefix), sealed...),
				&blob.WriterOptions{ContentType: "application/octet-stream"}); err != nil {
				return n, err
			}
		}
	}
}
