// Package templates loads JSON template definitions and validates user inputs against them.
package templates

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type Template struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Module      Module  `json:"module"`
	Inputs      []Input `json:"inputs"`
}

// Module points at an existing Terraform module at an approved revision.
type Module struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

type Input struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Help     string   `json:"help,omitempty"` // shown under the field, and as the error when Pattern doesn't match
	Type     string   `json:"type"`           // string | number | bool | select | list | string_list | map | object
	Required bool     `json:"required"`
	Pattern  string   `json:"pattern,omitempty"`
	Options  []Option `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
	// Sensitive values are stored sealed with SECRET_KEY, typed in a password field and never shown.
	Sensitive bool `json:"sensitive,omitempty"`
	// ShowIf hides the input unless an earlier select (or bool: "true" / "false") has one of the listed values. A hidden input is
	// not sent to Terraform: the module's default applies.
	ShowIf *ShowIf `json:"show_if,omitempty"`
	// Key: at most one input per template; its value names the template when it's added to a workspace
	// (module.<name>). The name is fixed then: changing the value later doesn't rename it.
	Key bool `json:"key,omitempty"`
	// PerEnvironment: a project ("environment") may set the value for all its workspaces, overriding theirs at every run.
	PerEnvironment bool `json:"per_environment,omitempty"`
	// Fields of a list input (a Terraform list(object({...}))), whose form has rows of them, or of an object input
	// (object({...})), one group of them. Each is a string, number, bool or select field; optional ones (Required
	// false) left empty are left out, so the type's default applies.
	Fields []Input `json:"fields,omitempty"`
}

// The complex types' form values are JSON: a list's an array of objects, a string_list's (list(string)) an array of
// strings, a map's (map(string)) an object of strings, an object's an object. The form also posts the rows of a
// string_list or map as objects (RowFields); Validate takes either.

// InputTypes are the input types; the JSON Schema's enum must match (TestSchema).
var InputTypes = []string{"string", "number", "bool", "select", "list", "string_list", "map", "object"}

// complexTypes are the input types whose value is JSON, never sensitive, the key or per-environment.
var complexTypes = []string{"list", "string_list", "map", "object"}

// RowFields are the fields of each row of a list, string_list (one: value) or map (key and value) input; nil for
// the other types.
func (in Input) RowFields() []Input {
	switch in.Type {
	case "list":
		return in.Fields
	case "string_list":
		return []Input{{Name: "value", Label: "Value", Type: "string", Pattern: in.Pattern, Help: in.Help}}
	case "map":
		return []Input{{Name: "key", Label: "Key", Type: "string", Required: true},
			{Name: "value", Label: "Value", Type: "string", Pattern: in.Pattern, Help: in.Help}}
	}
	return nil
}

// IsSealed reports whether a stored input value is a sealed sensitive one: {"sealed": "<base64>"}, that key only.
// A map or object input can't be stored with only that key (Validate), so it never passes for one.
func IsSealed(v any) bool {
	m, ok := v.(map[string]any)
	_, str := m["sealed"].(string)
	return ok && len(m) == 1 && str
}

// FieldTypes are the types a list's fields can have.
var FieldTypes = []string{"string", "number", "bool", "select"}

// Title is what the form calls the input: its label, else its name.
func (in Input) Title() string { return cmp.Or(in.Label, in.Name) }

// KeyInput returns the template's key input, nil without one.
func (t Template) KeyInput() *Input {
	for i := range t.Inputs {
		if t.Inputs[i].Key {
			return &t.Inputs[i]
		}
	}
	return nil
}

// ShowIf holds when the earlier select or bool input Input is shown and has one of the Equals values.
type ShowIf struct {
	Input  string   `json:"input"`
	Equals []string `json:"equals"`
}

func (c *ShowIf) holds(shown map[string]bool, cur map[string]string) bool {
	return c == nil || shown[c.Input] && slices.Contains(c.Equals, cur[c.Input])
}

// Option is a select choice: Label is shown, Value goes to Terraform. In JSON a plain string is both.
type Option struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

type plainOption Option // without the methods, so they don't recurse

func (o *Option) UnmarshalJSON(b []byte) error {
	if json.Unmarshal(b, &o.Value) == nil {
		return nil
	}
	if err := strict(b, (*plainOption)(o)); err != nil {
		return fmt.Errorf("option: %w", err)
	}
	return nil
}

func (o Option) MarshalJSON() ([]byte, error) {
	if o.Label == "" {
		return json.Marshal(o.Value)
	}
	return json.Marshal(plainOption(o))
}

// Text is what the form shows.
func (o Option) Text() string { return cmp.Or(o.Label, o.Value) }

func hasValue(opts []Option, v string) bool {
	return slices.ContainsFunc(opts, func(o Option) bool { return o.Value == v })
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`) // safe for use in state keys
	nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)        // valid Terraform variable name
	// Terraform forbids these as variable names; as inputs they'd set a module block's meta-arguments
	// (source: run other code with the workspace's credentials).
	reserved = []string{"source", "version", "providers", "count", "for_each", "lifecycle", "depends_on", "locals"}
)

// loadDir reads every *.json file in dir. Any invalid file fails the whole load.
func loadDir(dir string) (map[string]Template, map[string]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{}
	for _, f := range names {
		if files[f], err = os.ReadFile(f); err != nil {
			return nil, nil, err
		}
	}
	return Parse(files)
}

// Catalog limits, checked before parsing: far above real catalogs, far below the repository archive limits.
const (
	MaxCount     = 500
	MaxFileSize  = 256 << 10
	MaxTotalSize = 8 << 20
)

// Parse decodes and checks template files (path -> JSON). Any invalid file fails the whole set.
// It returns the templates by id, and each id's file path.
func Parse(files map[string][]byte) (map[string]Template, map[string]string, error) {
	if len(files) > MaxCount {
		return nil, nil, fmt.Errorf("%d template files: at most %d", len(files), MaxCount)
	}
	total := 0
	for _, f := range slices.Sorted(maps.Keys(files)) {
		if len(files[f]) > MaxFileSize {
			return nil, nil, fmt.Errorf("%s: %d bytes: a template file is at most %d", f, len(files[f]), MaxFileSize)
		}
		if total += len(files[f]); total > MaxTotalSize {
			return nil, nil, fmt.Errorf("template files total over %d bytes", MaxTotalSize)
		}
	}
	out, paths := map[string]Template{}, map[string]string{}
	for _, f := range slices.Sorted(maps.Keys(files)) { // a stable "duplicate" error
		var t Template
		if err := json.Unmarshal(files[f], &t); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := t.check(); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
		if _, dup := out[t.ID]; dup {
			return nil, nil, fmt.Errorf("%s: duplicate template id %q", f, t.ID)
		}
		out[t.ID], paths[t.ID] = t, f
	}
	return out, paths, nil
}

// Encode checks t and returns it as a template file.
func Encode(t Template) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(b) >= MaxFileSize { // the editor mustn't commit a file the next sync rejects
		return nil, fmt.Errorf("the template is %d bytes: at most %d", len(b), MaxFileSize)
	}
	return append(b, '\n'), nil
}

// Catalog is the current set of templates: from a directory (Save writes new or edited ones back
// as JSON to review and commit), or from a Git repository (Load swaps in each new commit's set).
type Catalog struct {
	Dir    string
	mu     sync.RWMutex
	m      map[string]Template
	paths  map[string]string // id -> file path (in Dir, or in the repository)
	mode   string            // "" the directory; a repository's editor mode: off | commit | pr
	commit string            // the repository commit the set is from
}

func Open(dir string) (*Catalog, error) {
	m, paths, err := loadDir(dir)
	if err != nil {
		return nil, err
	}
	return &Catalog{Dir: dir, m: m, paths: paths}, nil
}

// Load replaces the set with one from a repository commit (mode: its editor mode).
func (c *Catalog) Load(m map[string]Template, paths map[string]string, mode, commit string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m, c.paths, c.mode, c.commit = m, paths, mode, commit
}

// Reopen goes back to the directory's templates, when the repository is disconnected.
func (c *Catalog) Reopen() error {
	m, paths, err := loadDir(c.Dir)
	if err != nil {
		return err
	}
	c.Load(m, paths, "", "")
	return nil
}

// Source returns the repository editor mode ("" = the directory) and commit of the current set.
func (c *Catalog) Source() (mode, commit string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode, c.commit
}

// Path returns the file a template was loaded from.
func (c *Catalog) Path(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paths[id]
}

func (c *Catalog) Get(id string) (Template, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.m[id]
	return t, ok
}

// List returns all templates sorted by name.
func (c *Catalog) List() []Template {
	c.mu.RLock()
	defer c.mu.RUnlock()
	list := make([]Template, 0, len(c.m))
	for _, t := range c.m {
		list = append(list, t)
	}
	slices.SortFunc(list, func(a, b Template) int { return strings.Compare(a.Name, b.Name) })
	return list
}

// Save validates t and writes <dir>/<id>.json. Creating an id that already exists is an error.
func (c *Catalog) Save(t Template, overwrite bool) error {
	b, err := Encode(t)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != "" {
		return fmt.Errorf("the catalog is a Git repository: save through it")
	}
	if _, exists := c.m[t.ID]; exists && !overwrite {
		return fmt.Errorf("template %q already exists", t.ID)
	}
	// Write to a temp file and rename so a crash never leaves a half-written template.
	tmp, err := os.CreateTemp(c.Dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !idRe.MatchString(t.ID) { // Encode validates too; checked here so the path can't escape c.Dir
		return fmt.Errorf("invalid template id %q", t.ID)
	}
	path := filepath.Join(c.Dir, filepath.Base(t.ID)+".json")
	if p, ok := c.paths[t.ID]; ok {
		path = p // a hand-written file may be named otherwise
	}
	if rel, err := filepath.Rel(c.Dir, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("template path %q is outside the catalog", path)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	c.m[t.ID], c.paths[t.ID] = t, path
	return nil
}

// Delete removes template id's file from the directory.
func (c *Catalog) Delete(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != "" {
		return fmt.Errorf("the catalog is a Git repository: delete through it")
	}
	p, ok := c.paths[id]
	if !ok {
		return fmt.Errorf("template %q does not exist", id)
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	delete(c.m, id)
	delete(c.paths, id)
	return nil
}

func (t Template) check() error {
	if !idRe.MatchString(t.ID) || t.ID == "new" { // "new" is the builder route
		return fmt.Errorf("invalid id %q (lowercase letters, digits and dashes)", t.ID)
	}
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if t.Module.Source == "" || t.Module.Ref == "" {
		return fmt.Errorf("module.source and module.ref are required")
	}
	before := map[string]Input{} // show_if may only refer to these
	for _, in := range t.Inputs {
		if _, dup := before[in.Name]; dup {
			return fmt.Errorf("duplicate input %q", in.Name)
		}
		if !nameRe.MatchString(in.Name) || slices.Contains(reserved, in.Name) {
			return fmt.Errorf("invalid input name %q (a Terraform variable name: lowercase, digits, underscores; not %s)",
				in.Name, strings.Join(reserved, ", "))
		}
		if !slices.Contains(InputTypes, in.Type) {
			return fmt.Errorf("input %q: unknown type %q (%s)", in.Name, in.Type, strings.Join(InputTypes, ", "))
		}
		if in.Type == "select" && len(in.Options) == 0 {
			return fmt.Errorf("input %q: select needs options", in.Name)
		}
		if err := in.checkList(); err != nil {
			return err
		}
		if in.Sensitive && in.Type != "string" && in.Type != "number" {
			return fmt.Errorf("input %q: only string and number inputs can be sensitive", in.Name)
		}
		if in.Key && (in.Type == "bool" || in.Sensitive) { // a name shows its value
			return fmt.Errorf("input %q: the key can't be a yes / no or sensitive input", in.Name)
		}
		if k := t.KeyInput(); in.Key && k.Name != in.Name {
			return fmt.Errorf("input %q: only one input can be the key, %q is", in.Name, k.Name)
		}
		if in.PerEnvironment && in.Sensitive { // project values are stored and shown in plain text
			return fmt.Errorf("template %q, input %q: a per-environment input can't be sensitive", t.ID, in.Name)
		}
		if in.PerEnvironment && in.Key { // the key names the module, fixed when added
			return fmt.Errorf("template %q, input %q: a per-environment input can't be the key", t.ID, in.Name)
		}
		if in.Sensitive && in.Default != "" { // the template file is plain text
			return fmt.Errorf("input %q: a sensitive input can't have a default", in.Name)
		}
		if in.Pattern != "" { // compiled as Validate does: near the size limits, the anchored form may fail alone
			if _, err := regexp.Compile(anchored(in.Pattern)); err != nil {
				return fmt.Errorf("input %q: %w", in.Name, err)
			}
		}
		if err := in.ShowIf.check(before); err != nil {
			return fmt.Errorf("input %q: %w", in.Name, err)
		}
		for _, o := range in.Options {
			if o.Value == "" {
				return fmt.Errorf("input %q: an option needs a value", in.Name)
			}
		}
		if in.Type == "select" && in.Default != "" && !hasValue(in.Options, in.Default) {
			return fmt.Errorf("input %q: default %q is not one of the options", in.Name, in.Default)
		}
		if in.Type == "bool" && in.Default != "" && in.Default != "true" && in.Default != "false" {
			return fmt.Errorf("input %q: a bool default is true or false", in.Name)
		}
		if in.Default != "" && in.Type != "select" { // the deploy form starts with the default: it must pass the same checks
			d := in
			d.ShowIf = nil // alone, its condition's input is missing
			if _, errs := (Template{Inputs: []Input{d}}).Validate(map[string]string{in.Name: in.Default}); errs[in.Name] != "" {
				return fmt.Errorf("input %q: default %q: %s", in.Name, in.Default, errs[in.Name])
			}
		}
		before[in.Name] = in
	}
	return nil
}

// checkList checks a complex input: its fields (a list's or object's), and that it's none of what it can't be (yet).
func (in Input) checkList() error {
	hasFields := in.Type == "list" || in.Type == "object"
	if !hasFields && len(in.Fields) > 0 {
		return fmt.Errorf("input %q: only a list or an object has fields", in.Name)
	}
	if !slices.Contains(complexTypes, in.Type) {
		return nil
	}
	if in.Sensitive || in.Key || in.PerEnvironment {
		return fmt.Errorf("input %q: a %s can't be sensitive, the key or per-environment", in.Name, in.Type)
	}
	if hasFields && len(in.Fields) == 0 {
		return fmt.Errorf("input %q: a %s needs fields", in.Name, in.Type)
	}
	names := map[string]bool{}
	for _, f := range in.Fields {
		switch {
		case !nameRe.MatchString(f.Name) || names[f.Name]:
			return fmt.Errorf("input %q: invalid or duplicate field name %q", in.Name, f.Name)
		case !slices.Contains(FieldTypes, f.Type):
			return fmt.Errorf("input %q, field %q: unknown type %q (%s)", in.Name, f.Name, f.Type, strings.Join(FieldTypes, ", "))
		case f.Sensitive || f.Key || f.PerEnvironment || f.ShowIf != nil || len(f.Fields) > 0:
			return fmt.Errorf("input %q, field %q: a field can't be sensitive, the key, per-environment, shown conditionally or a list", in.Name, f.Name)
		}
		names[f.Name] = true
		if err := (Template{ID: "x", Name: "x", Module: Module{"x", "x"}, Inputs: []Input{f}}).check(); err != nil {
			return fmt.Errorf("input %q, field %s", in.Name, strings.TrimPrefix(err.Error(), "input "))
		}
	}
	return nil
}

func (c *ShowIf) check(before map[string]Input) error {
	if c == nil {
		return nil
	}
	in, ok := before[c.Input]
	if !ok || in.Type != "select" && in.Type != "bool" {
		return fmt.Errorf("show when: %q is not a dropdown or yes / no field above this field", c.Input)
	}
	if len(c.Equals) == 0 {
		return fmt.Errorf("show when %s: pick at least one value", c.Input)
	}
	opts := in.Options
	if in.Type == "bool" {
		opts = []Option{{Value: "true"}, {Value: "false"}}
	}
	for _, v := range c.Equals {
		if !hasValue(opts, v) {
			return fmt.Errorf("show when: %q is not an option of %q", v, c.Input)
		}
	}
	return nil
}

// eval returns which inputs the form values raw show, and their values.
func (t Template) eval(raw map[string]string) (shown map[string]bool, cur map[string]string) {
	shown, cur = map[string]bool{}, map[string]string{}
	for _, in := range t.Inputs {
		v := strings.TrimSpace(raw[in.Name])
		if in.Type == "select" && v == "" && len(in.Options) > 0 {
			v = in.Options[0].Value // what the browser shows selected
		}
		if in.Type == "bool" { // a checkbox posts "on", a stored value or default reads "true"
			v = strconv.FormatBool(v == "on" || v == "true")
		}
		cur[in.Name] = v
		shown[in.Name] = in.ShowIf.holds(shown, cur)
	}
	return shown, cur
}

// Match reports whether the show_if c (nil: none) holds for the form values raw.
func (t Template) Match(c *ShowIf, raw map[string]string) bool {
	shown, cur := t.eval(raw)
	return c.holds(shown, cur)
}

func anchored(pattern string) string { return "^(?:" + pattern + ")$" }

// StoredValue is a stored input as the form asked it: the question and the answer in its words.
type StoredValue struct {
	Name, Title, Text string
	Sensitive         bool   // sealed: the text is never shown
	Project           string // set by this project (a per-environment input): Text is its value, not the stored one
}

// Describe lists the stored inputs in the form's order, then any the template no longer asks (by name).
func (t Template) Describe(stored map[string]any) []StoredValue {
	var out []StoredValue
	add := func(in Input, v any) {
		sv := StoredValue{Name: in.Name, Title: in.Title()}
		switch x := v.(type) {
		case bool:
			sv.Text = map[bool]string{true: "Yes", false: "No"}[x]
		case []any: // a list: a row per line, its fields in the form's words
			var rows []string
			for _, r := range x {
				row, isObj := r.(map[string]any)
				if !isObj { // a string_list's
					rows = append(rows, FormatValue(r))
					continue
				}
				rows = append(rows, in.describeFields(row))
			}
			sv.Text = cmp.Or(strings.Join(rows, "\n"), "(none)")
		case map[string]any:
			if IsSealed(x) {
				sv.Sensitive = true
				break
			}
			if in.Type == "object" { // its fields on a line
				sv.Text = cmp.Or(in.describeFields(x), "(none)")
				break
			}
			var rows []string // a map: a key per line
			for _, k := range slices.Sorted(maps.Keys(x)) {
				rows = append(rows, k+": "+FormatValue(x[k]))
			}
			sv.Text = cmp.Or(strings.Join(rows, "\n"), "(none)")
		default:
			sv.Text = FormatValue(v)
			for _, o := range in.Options {
				if o.Value == sv.Text {
					sv.Text = o.Text()
				}
			}
		}
		out = append(out, sv)
	}
	asked := map[string]bool{}
	for _, in := range t.Inputs {
		if v, ok := stored[in.Name]; ok {
			add(in, v)
			asked[in.Name] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(stored)) {
		if !asked[k] {
			add(Input{Name: k}, stored[k])
		}
	}
	return out
}

// describeFields shows an object (a list's row, or an object input's value) in the form's words: Field: value, ….
func (in Input) describeFields(row map[string]any) string {
	var parts []string
	for _, f := range in.Fields {
		if v, ok := row[f.Name]; ok && v != nil {
			parts = append(parts, f.Title()+": "+FormatValue(v))
		}
	}
	return strings.Join(parts, ", ")
}

// FormatValue shows a stored input value or module default as typed: fmt prints the float64 1000000 as 1e+06.
// A list (or object) is its JSON, the form's value of a list input.
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil: // an input left empty while importing (stored null)
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any, map[string]any:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return fmt.Sprint(v)
}

// Validate turns raw form values into typed Terraform variables.
// It returns per-field error messages; an empty map means valid. Hidden inputs are left out.
func (t Template) Validate(raw map[string]string) (map[string]any, map[string]string) {
	vals, errs := map[string]any{}, map[string]string{}
	shown, _ := t.eval(raw)
	for _, in := range t.Inputs {
		if !shown[in.Name] {
			continue
		}
		v := raw[in.Name]
		// a password may start or end with a space, but only spaces is empty: a sensitive input keeps its stored value
		if !in.Sensitive || in.Type != "string" || strings.TrimSpace(v) == "" {
			v = strings.TrimSpace(v)
		}
		if in.Type == "bool" {
			vals[in.Name] = v == "on" || v == "true"
			continue
		}
		if slices.Contains(complexTypes, in.Type) {
			val, msg := in.validateComplex(v)
			if msg != "" {
				errs[in.Name] = msg
				continue
			}
			vals[in.Name] = val
			continue
		}
		if v == "" {
			if in.Required {
				errs[in.Name] = "This field is required."
			}
			continue
		}
		switch in.Type {
		case "number":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) { // NaN and Inf parse, but aren't JSON
				errs[in.Name] = "Enter a number, e.g. 42."
				continue
			}
			vals[in.Name] = n
		case "select":
			if !hasValue(in.Options, v) {
				errs[in.Name] = "Choose one of the options."
				continue
			}
			vals[in.Name] = v
		default:
			// anchored like the browser's pattern attribute: the whole value must match
			if in.Pattern != "" && !regexp.MustCompile(anchored(in.Pattern)).MatchString(v) {
				errs[in.Name] = cmp.Or(in.Help, "Doesn't have the expected format: "+in.Pattern) // the help says it in words
				continue
			}
			vals[in.Name] = v
		}
	}
	return vals, errs
}

// validateComplex checks a complex input's form value (JSON, "" none; see InputTypes), and returns its typed value
// or the first error. A list's rows and an object are checked field by field, each as an input of its own,
// optional fields left empty left out; a string_list's and map's rows may be objects (RowFields) too.
func (in Input) validateComplex(v string) (any, string) {
	var x any
	if v != "" && json.Unmarshal([]byte(v), &x) != nil {
		return nil, "Enter a valid value."
	}
	if in.Type == "object" {
		obj, ok := x.(map[string]any)
		if !ok && x != nil {
			return nil, "Enter the fields."
		}
		val, msg := in.validateFields(obj, "")
		if len(val) == 1 && IsSealed(val) { // would pass for a sealed value
			return nil, "An object with only the field sealed can't be stored."
		}
		return val, msg
	}
	var rows []any
	switch y := x.(type) {
	case nil:
	case []any:
		rows = y
	case map[string]any: // a map as stored: a row per key
		if in.Type != "map" {
			return nil, "Enter a list of rows."
		}
		for _, k := range slices.Sorted(maps.Keys(y)) {
			rows = append(rows, map[string]any{"key": k, "value": y[k]})
		}
	default:
		return nil, "Enter a list of rows."
	}
	list, m := []any{}, map[string]any{}
	for i, r := range rows {
		row, ok := r.(map[string]any)
		if !ok && in.Type == "string_list" { // as stored: a string per element
			row, ok = map[string]any{"value": r}, true
		}
		if !ok {
			return nil, "Enter a list of rows."
		}
		val, msg := in.validateFields(row, fmt.Sprintf("Row %d, ", i+1))
		switch {
		case msg != "":
			return nil, msg
		case in.Type == "list":
			list = append(list, val)
		case in.Type == "string_list" && val["value"] != nil:
			list = append(list, val["value"])
		case in.Type == "map" && val["value"] != nil:
			k, _ := val["key"].(string)
			if _, dup := m[k]; dup {
				return nil, fmt.Sprintf("Row %d, Key: %s is there already.", i+1, k)
			}
			m[k] = val["value"]
		}
	}
	if in.Type == "map" {
		if IsSealed(m) { // would pass for a sealed value
			return nil, "A map with only the key sealed can't be stored."
		}
		return m, ""
	}
	return list, ""
}

// validateFields checks an object (a row of RowFields, or an object input's value) field by field, each as an
// input of its own, and returns its typed fields, or the first error, prefixed by where.
func (in Input) validateFields(obj map[string]any, where string) (map[string]any, string) {
	fields := in.RowFields()
	if in.Type == "object" {
		fields = in.Fields
	}
	raw := map[string]string{}
	for k, x := range obj {
		if x != nil {
			raw[k] = FormatValue(x)
		}
	}
	vals, errs := Template{Inputs: fields}.Validate(raw)
	for _, f := range fields {
		if e := errs[f.Name]; e != "" {
			return nil, where + f.Title() + ": " + e
		}
	}
	return vals, ""
}
