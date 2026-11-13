package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
	"github.com/opsolete/opsolete/internal/templates"
)

// Sensitive template inputs are stored in a workspace's inputs as {"sealed": "<base64 secrets.Seal>"}: that key
// only (templates.IsSealed), which a map or object input's value can't be.

func IsSealed(v any) bool { return templates.IsSealed(v) }

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
		if !IsSealed(v) { // plain, a map or object input's too
			continue
		}
		str, _ := v.(map[string]any)["sealed"].(string)
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

// formSave is one save of a template module's inputs, the one pipeline of every surface that saves them: the
// add and configure forms, a new workspace, a promotion and a rollout.
type formSave struct {
	T         templates.Template // resolved at the module version it saves
	Raw       map[string]string  // the form's values by input name (sensitive ones as typed, "" to keep)
	Old       map[string]any     // the module's stored inputs; nil for a new module
	Follows   []string           // its per-environment inputs that followed the project (storage.WorkspaceTemplate.Follows)
	Found     map[string]any     // what an import plan found that the form took: the module's own
	Env       EnvironmentMap     // its project's values
	Workspace uuid.UUID          // sensitive inputs are sealed to the workspace and module
	Module    string
}

// saved is a formSave worked out.
type saved struct {
	Vals    map[string]any    // the inputs to store
	Follows []string          // the per-environment inputs locked to the project's value, which it follows
	Raw     map[string]string // the form's values with the project's in place: what show_if sees
	Locked  map[string]string // the project's values (EnvironmentMap.Locked)
	Errs    map[string]string // by input name; empty: it may be stored
	Import  bool              // the module imports what exists (the form's import switch, not stored as an input)
}

// save locks the inputs the project sets (their posted values are ignored, the module keeps its own), validates
// the rest (only the key input required while importing), seals the sensitive ones (empty keeps the stored value),
// records which inputs follow the project, and takes the import switch out of the inputs (saved.Import).
func (a *App) save(s formSave) (saved, error) {
	locked := s.Env.LockedForm(s.T, s.Old, s.Follows, s.Found)
	raw := lockInputs(s.Raw, locked)
	vals, errs := validate(s.T, raw)
	if err := a.SealInputs(s.T, raw, vals, s.Old, errs, s.Workspace, s.Module); err != nil {
		return saved{}, err
	}
	keepOwn(vals, errs, locked, s.Old)
	imp := Importing(s.T, raw)
	delete(vals, ImportInput) // a property of the module (storage.WorkspaceTemplate.Import); the runner gets it
	return saved{Vals: vals, Follows: slices.Sorted(maps.Keys(locked)), Raw: raw, Locked: locked, Errs: errs, Import: imp}, nil
}

// plainSecrets are the values of m's inputs its template now marks sensitive but that were stored before it
// did, in plain text: they mask like sealed ones until the form saves them sealed.
func (a *App) plainSecrets(m storage.WorkspaceTemplate) []string {
	if a.Templates == nil {
		return nil
	}
	t, ok := a.Templates.Get(m.TemplateID)
	if !ok {
		return nil
	}
	var out []string
	for _, in := range t.Inputs {
		if v, set := m.Inputs[in.Name]; set && in.Sensitive && v != nil && !IsSealed(v) {
			out = append(out, templates.FormatValue(v))
		}
	}
	return out
}
