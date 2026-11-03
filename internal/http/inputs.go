package httpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"

	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// Sensitive template inputs are stored in a workspace's inputs as {"sealed": "<base64 AES-GCM>"}.
// A form value is a string, number or bool, never an object, so it can't pass for a sealed one.

// secretPrefix names the form fields of sensitive inputs, so the audit log redacts them.
const secretPrefix = "secret."

// inputAD binds a sealed input to its workspace, its module ("" = root) and its name.
func inputAD(workspaceID, module, name string) string {
	return "template input\x00" + workspaceID + "\x00" + module + "\x00" + name
}

func isSealed(v any) bool { _, ok := v.(map[string]any); return ok }

// formValues reads t's inputs from a posted form.
func formValues(r *http.Request, t templates.Template) map[string]string {
	raw := map[string]string{}
	for _, in := range t.Inputs {
		field := in.Name
		if in.Sensitive {
			field = secretPrefix + in.Name
		}
		raw[in.Name] = r.PostForm.Get(field)
	}
	return raw
}

// sealInputs seals t's sensitive inputs in vals, validated from the form values raw, for workspace
// wsID's module. One left empty keeps its stored value in old (sealing it if it was stored before it
// became sensitive); one the form hides is dropped.
func (s *Server) sealInputs(t templates.Template, raw map[string]string, vals, old map[string]any, errs map[string]string, wsID, module string) error {
	for _, in := range t.Inputs {
		if !in.Sensitive || !t.Match(in.ShowIf, raw) {
			continue
		}
		v, ok := vals[in.Name]
		if !ok {
			if v, ok = old[in.Name]; !ok {
				continue
			}
			delete(errs, in.Name) // "required": it has a value
		}
		if isSealed(v) {
			vals[in.Name] = v
			continue
		}
		if s.SecretKey == nil {
			return errors.New("sensitive inputs are stored encrypted: set SECRET_KEY first")
		}
		plain, err := json.Marshal(v)
		if err != nil {
			return err
		}
		sealed, err := vcs.Seal(s.SecretKey, string(plain), inputAD(wsID, module, in.Name))
		if err != nil {
			return err
		}
		vals[in.Name] = map[string]any{"sealed": base64.StdEncoding.EncodeToString(sealed)}
	}
	return nil
}

// openInputs returns inputs with their sealed values decrypted, for the runner.
func (s *Server) openInputs(inputs map[string]any, wsID, module string) (map[string]any, error) {
	out := maps.Clone(inputs)
	for k, v := range inputs {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		str, _ := m["sealed"].(string)
		sealed, err := base64.StdEncoding.DecodeString(str)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", k, err)
		}
		plain, err := vcs.Open(s.SecretKey, sealed, inputAD(wsID, module, k))
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", k, err)
		}
		var val any
		if err := json.Unmarshal([]byte(plain), &val); err != nil {
			return nil, fmt.Errorf("input %s: %w", k, err)
		}
		out[k] = val
	}
	return out, nil
}

// formDefaults fills a form from stored inputs: sensitive ones stay empty, set lists those with a value.
func formDefaults(t templates.Template, stored map[string]any) (vals map[string]string, set map[string]bool) {
	vals, set = map[string]string{}, map[string]bool{}
	for _, in := range t.Inputs {
		v, ok := stored[in.Name]
		switch {
		case in.Sensitive:
			set[in.Name] = ok
		case ok && !isSealed(v): // a sealed value of a no longer sensitive input must be typed again
			vals[in.Name] = fmt.Sprint(v)
		case !ok:
			vals[in.Name] = in.Default
		}
	}
	return vals, set
}
