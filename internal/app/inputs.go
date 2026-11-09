package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/templates"
)

// Sensitive template inputs are stored in a workspace's inputs as {"sealed": "<base64 secrets.Seal>"}.
// A form value is a string, number or bool, never an object, so it can't pass for a sealed one.

func IsSealed(v any) bool { _, ok := v.(map[string]any); return ok }

// FormDefaults fills a form from stored inputs: sensitive ones stay empty, set lists those with a value.
func FormDefaults(t templates.Template, stored map[string]any) (vals map[string]string, set map[string]bool) {
	vals, set = map[string]string{}, map[string]bool{}
	for _, in := range t.Inputs {
		v, ok := stored[in.Name]
		switch {
		case in.Sensitive:
			set[in.Name] = ok
		case ok && !IsSealed(v): // a sealed value of a no longer sensitive input must be typed again
			vals[in.Name] = templates.FormatValue(v)
		case !ok:
			vals[in.Name] = in.Default
		}
	}
	return vals, set
}

// SealInputs seals t's sensitive inputs in vals, validated from the form values raw, for workspace
// wsID's module. One left empty keeps its stored value in old (sealing it if it was stored before it
// became sensitive); one the form hides is dropped.
func (a *App) SealInputs(t templates.Template, raw map[string]string, vals, old map[string]any, errs map[string]string, wsID uuid.UUID, module string) error {
	for _, in := range t.Inputs {
		if !in.Sensitive || !t.Match(in.ShowIf, raw) {
			continue
		}
		v, ok := vals[in.Name]
		if !ok {
			if strings.TrimSpace(raw[in.Name]) != "" {
				continue // a new value Validate rejected: keep its error, not the stored value
			}
			if v, ok = old[in.Name]; !ok {
				continue
			}
			delete(errs, in.Name) // "required": it has a value
		}
		if IsSealed(v) {
			vals[in.Name] = v
			continue
		}
		plain, err := json.Marshal(v)
		if err != nil {
			return err
		}
		sealed, err := a.Secrets.Seal(string(plain), InputAD(wsID, module, in.Name))
		if err != nil {
			return err
		}
		vals[in.Name] = map[string]any{"sealed": base64.StdEncoding.EncodeToString(sealed)}
	}
	return nil
}

// OpenInputs returns inputs with their sealed values decrypted, for the runner.
func (a *App) OpenInputs(inputs map[string]any, wsID uuid.UUID, module string) (map[string]any, []string, error) {
	var secrets []string // the opened values, for the runner to mask in the log
	out := maps.Clone(inputs)
	for k, v := range inputs {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		str, _ := m["sealed"].(string)
		sealed, err := base64.StdEncoding.DecodeString(str)
		if err != nil {
			return nil, nil, fmt.Errorf("input %s: %w", k, err)
		}
		plain, err := a.Secrets.Open(sealed, InputAD(wsID, module, k))
		if err != nil {
			return nil, nil, fmt.Errorf("input %s: %w", k, err)
		}
		var val any
		if err := json.Unmarshal([]byte(plain), &val); err != nil {
			return nil, nil, fmt.Errorf("input %s: %w", k, err)
		}
		if str, ok := val.(string); ok { // shortcut: only string values are masked; numbers and lists print in other forms
			secrets = append(secrets, str)
		}
		out[k] = val
	}
	return out, secrets, nil
}
