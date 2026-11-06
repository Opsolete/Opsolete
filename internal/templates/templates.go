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
	Type     string   `json:"type"` // string | number | bool | select
	Required bool     `json:"required"`
	Pattern  string   `json:"pattern,omitempty"`
	Options  []Option `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
	// Sensitive values are stored sealed with SECRET_KEY, typed in a password field and never shown.
	Sensitive bool `json:"sensitive,omitempty"`
	// ShowIf hides the input unless an earlier select has one of the listed values. A hidden input is
	// not sent to Terraform: the module's default applies.
	ShowIf *ShowIf `json:"show_if,omitempty"`
	// Key: at most one input per template; its value names the template when it's added to a workspace
	// (module.<name>). The name is fixed then: changing the value later doesn't rename it.
	Key bool `json:"key,omitempty"`
}

// Title and Help split the label at its first ": ", as module variable descriptions often read
// ("VM name: 1-64 letters…"): the form shows Title and puts Help behind a ?.
func (in Input) Title() string { t, _, _ := strings.Cut(in.Label, ": "); return cmp.Or(t, in.Name) }
func (in Input) Help() string  { _, h, _ := strings.Cut(in.Label, ": "); return h }

// KeyInput returns the template's key input, nil without one.
func (t Template) KeyInput() *Input {
	for i := range t.Inputs {
		if t.Inputs[i].Key {
			return &t.Inputs[i]
		}
	}
	return nil
}

// ShowIf holds when the earlier select input Input is shown and has one of the Equals values.
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
	return json.Unmarshal(b, (*plainOption)(o))
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
	path := filepath.Join(c.Dir, t.ID+".json")
	if p, ok := c.paths[t.ID]; ok {
		path = p // a hand-written file may be named otherwise
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
		switch in.Type {
		case "string", "number", "bool":
		case "select":
			if len(in.Options) == 0 {
				return fmt.Errorf("input %q: select needs options", in.Name)
			}
		default:
			return fmt.Errorf("input %q: unknown type %q", in.Name, in.Type)
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
				return fmt.Errorf("input %q: default %q %s", in.Name, in.Default, errs[in.Name])
			}
		}
		before[in.Name] = in
	}
	return nil
}

func (c *ShowIf) check(before map[string]Input) error {
	if c == nil {
		return nil
	}
	if in, ok := before[c.Input]; !ok || in.Type != "select" {
		return fmt.Errorf("show when: %q is not a dropdown above this field", c.Input)
	}
	if len(c.Equals) == 0 {
		return fmt.Errorf("show when %s: pick at least one value", c.Input)
	}
	for _, v := range c.Equals {
		if !hasValue(before[c.Input].Options, v) {
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

// FormatValue shows a stored input value or module default as typed: fmt prints the float64 1000000 as 1e+06.
func FormatValue(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
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
		if !in.Sensitive || in.Type != "string" { // a password may start or end with a space
			v = strings.TrimSpace(v)
		}
		if in.Type == "bool" {
			vals[in.Name] = v == "on" || v == "true"
			continue
		}
		if v == "" {
			if in.Required {
				errs[in.Name] = "required"
			}
			continue
		}
		switch in.Type {
		case "number":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) { // NaN and Inf parse, but aren't JSON
				errs[in.Name] = "must be a number"
				continue
			}
			vals[in.Name] = n
		case "select":
			if !hasValue(in.Options, v) {
				errs[in.Name] = "not an allowed option"
				continue
			}
			vals[in.Name] = v
		default:
			// anchored like the browser's pattern attribute: the whole value must match
			if in.Pattern != "" && !regexp.MustCompile(anchored(in.Pattern)).MatchString(v) {
				errs[in.Name] = "must match " + in.Pattern
				continue
			}
			vals[in.Name] = v
		}
	}
	return vals, errs
}
