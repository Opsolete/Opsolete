package templates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// SchemaURL is the JSON Schema of the template format (docs/static/schema/template-1.json), written into every file Encode makes.
const SchemaURL = "https://opsolete.github.io/Opsolete/schema/template-1.json"

// The template file format. In a file, default and show_if.equals are typed by the input's type
// (string/select: string, number: number, bool: boolean); in Go they stay the form's strings ("7", "true").
// The conversion happens only here, in Template's UnmarshalJSON and MarshalJSON.
type (
	fileTemplate struct {
		Schema      string            `json:"$schema,omitempty"` // allowed, ignored
		ID          string            `json:"id"`
		Name        string            `json:"name"`
		Description string            `json:"description"`
		Module      json.RawMessage   `json:"module"`
		Inputs      []json.RawMessage `json:"inputs"`
	}
	fileInput struct {
		Name      string          `json:"name"`
		Label     string          `json:"label"`
		Help      string          `json:"help,omitempty"`
		Type      string          `json:"type"`
		Required  bool            `json:"required"`
		Pattern   string          `json:"pattern,omitempty"`
		Options   []Option        `json:"options,omitempty"`
		Default   json.RawMessage `json:"default,omitempty"`
		Sensitive bool            `json:"sensitive,omitempty"`
		ShowIf    *fileShowIf     `json:"show_if,omitempty"`
		Key       bool            `json:"key,omitempty"`
		PerEnv    bool            `json:"per_environment,omitempty"`
	}
	fileShowIf struct {
		Input  string            `json:"input"`
		Equals []json.RawMessage `json:"equals"`
	}
)

// strict decodes b into v, failing on unknown fields.
func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

func (t *Template) UnmarshalJSON(b []byte) error {
	var id struct{ ID any } // for the error messages, whatever else is wrong
	json.Unmarshal(b, &id)
	where := fmt.Sprintf("template %v", inputRef(id.ID, -1))
	var f fileTemplate
	if err := strict(b, &f); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	out := Template{ID: f.ID, Name: f.Name, Description: f.Description}
	if f.Module != nil {
		if err := strict(f.Module, &out.Module); err != nil {
			return fmt.Errorf("%s: module: %w", where, err)
		}
	}
	files := make([]fileInput, len(f.Inputs))
	types := map[string]string{}
	for i, raw := range f.Inputs {
		var name struct{ Name any }
		json.Unmarshal(raw, &name)
		if err := strict(raw, &files[i]); err != nil {
			return fmt.Errorf("%s, input %v: %w", where, inputRef(name.Name, i), err)
		}
		types[files[i].Name] = files[i].Type
	}
	for i, fi := range files {
		in := Input{Name: fi.Name, Label: fi.Label, Help: fi.Help, Type: fi.Type, Required: fi.Required, Pattern: fi.Pattern,
			Options: fi.Options, Sensitive: fi.Sensitive, Key: fi.Key, PerEnvironment: fi.PerEnv}
		where := fmt.Sprintf("%s, input %v", where, inputRef(fi.Name, i))
		var err error
		if fi.Default != nil {
			if in.Default, err = fromJSON(fi.Type, fi.Default); err != nil {
				return fmt.Errorf("%s: default %w", where, err)
			}
		}
		if fi.ShowIf != nil {
			in.ShowIf = &ShowIf{Input: fi.ShowIf.Input}
			for _, raw := range fi.ShowIf.Equals {
				v, err := fromJSON(types[fi.ShowIf.Input], raw) // typed by the input it refers to
				if err != nil {
					return fmt.Errorf("%s: show_if.equals: each value %w", where, err)
				}
				in.ShowIf.Equals = append(in.ShowIf.Equals, v)
			}
		}
		out.Inputs = append(out.Inputs, in)
	}
	*t = out
	return nil
}

func (t Template) MarshalJSON() ([]byte, error) {
	type showIf struct {
		Input  string `json:"input"`
		Equals []any  `json:"equals"`
	}
	type input struct {
		fileInput
		Default any     `json:"default,omitempty"`
		ShowIf  *showIf `json:"show_if,omitempty"`
	}
	module, err := json.Marshal(t.Module)
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	inputs := make([]input, len(t.Inputs))
	for i, in := range t.Inputs {
		types[in.Name] = in.Type
		inputs[i].fileInput = fileInput{Name: in.Name, Label: in.Label, Help: in.Help, Type: in.Type, Required: in.Required,
			Pattern: in.Pattern, Options: in.Options, Sensitive: in.Sensitive, Key: in.Key, PerEnv: in.PerEnvironment}
		if in.Default != "" {
			if inputs[i].Default, err = toJSON(in.Type, in.Default); err != nil {
				return nil, fmt.Errorf("input %s: default: %w", in.Name, err)
			}
		}
	}
	for i, in := range t.Inputs {
		if in.ShowIf == nil {
			continue
		}
		inputs[i].ShowIf = &showIf{Input: in.ShowIf.Input, Equals: []any{}}
		for _, v := range in.ShowIf.Equals {
			j, err := toJSON(types[in.ShowIf.Input], v)
			if err != nil {
				return nil, fmt.Errorf("input %s: show_if.equals: %w", in.Name, err)
			}
			inputs[i].ShowIf.Equals = append(inputs[i].ShowIf.Equals, j)
		}
	}
	return json.Marshal(struct {
		Schema      string          `json:"$schema"`
		ID          string          `json:"id"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Module      json.RawMessage `json:"module"`
		Inputs      []input         `json:"inputs"`
	}{SchemaURL, t.ID, t.Name, t.Description, module, inputs})
}

// inputRef names an input (i -1: the template) in errors: its name, else its position.
func inputRef(name any, i int) string {
	if s, ok := name.(string); ok && s != "" {
		return s
	}
	if i < 0 {
		return "without an id"
	}
	return fmt.Sprintf("#%d", i+1)
}

// fromJSON reads a value of an input of type typ as the form's string; the error reads after "default" and says what to write.
func fromJSON(typ string, raw json.RawMessage) (string, error) {
	var s string
	isString := json.Unmarshal(raw, &s) == nil
	switch typ {
	case "number":
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			return FormatValue(f), nil
		}
		if isString && json.Unmarshal([]byte(s), &f) == nil {
			return "", fmt.Errorf("must be a JSON number (%s, not %s)", s, raw)
		}
		return "", fmt.Errorf("must be a JSON number, not %s", raw)
	case "bool":
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return strconv.FormatBool(b), nil
		}
		if isString && (s == "true" || s == "false") {
			return "", fmt.Errorf("must be a JSON boolean (%s, not %s)", s, raw)
		}
		return "", fmt.Errorf("must be a JSON boolean (true or false), not %s", raw)
	}
	if isString { // string, select, or an unknown type check reports
		return s, nil
	}
	if bytes.Equal(raw, []byte("null")) {
		return "", errors.New("must be a JSON string, not null")
	}
	return "", fmt.Errorf("must be a JSON string (%q, not %s)", raw, raw)
}

// toJSON is fromJSON's inverse: the typed value to write for the form's string v.
func toJSON(typ, v string) (any, error) {
	switch typ {
	case "number":
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%q is not a finite number", v)
		}
		return f, nil
	case "bool":
		b, err := strconv.ParseBool(v)
		if err != nil || v != "true" && v != "false" {
			return nil, fmt.Errorf("%q is not true or false", v)
		}
		return b, nil
	}
	return v, nil
}
