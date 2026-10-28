// Package templates loads JSON template definitions and validates user inputs against them.
package templates

import (
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
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`) // safe for use in state keys
	nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)        // valid Terraform variable name
	// Terraform forbids these as variable names; as inputs they'd set a module block's meta-arguments
	// (source: run other code with the workspace's credentials).
	reserved = []string{"source", "version", "providers", "count", "for_each", "lifecycle", "depends_on", "locals"}
)

// LoadDir reads every *.json file in dir. Any invalid file fails the whole load.
func LoadDir(dir string) (map[string]Template, error) {
	m, _, err := loadDir(dir)
	return m, err
}

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
	if len(b) >= MaxFileSize { // the editor mustn't commit a file the next sync rejects
		return nil, fmt.Errorf("the template is %d bytes: at most %d", len(b), MaxFileSize)
	}
	return append(b, '\n'), err
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
	seen := map[string]bool{}
	for _, in := range t.Inputs {
		if seen[in.Name] {
			return fmt.Errorf("duplicate input %q", in.Name)
		}
		seen[in.Name] = true
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
		if in.Pattern != "" {
			if _, err := regexp.Compile(in.Pattern); err != nil {
				return fmt.Errorf("input %q: %w", in.Name, err)
			}
		}
		if in.Type == "select" && in.Default != "" && !slices.Contains(in.Options, in.Default) {
			return fmt.Errorf("input %q: default %q is not one of the options", in.Name, in.Default)
		}
		if in.Type == "bool" && in.Default != "" && in.Default != "true" && in.Default != "false" {
			return fmt.Errorf("input %q: a bool default is true or false", in.Name)
		}
		if in.Default != "" { // the deploy form starts with the default: it must pass the same checks
			if _, errs := (Template{Inputs: []Input{in}}).Validate(map[string]string{in.Name: in.Default}); errs[in.Name] != "" {
				return fmt.Errorf("input %q: default %q %s", in.Name, in.Default, errs[in.Name])
			}
		}
	}
	return nil
}

// Validate turns raw form values into typed Terraform variables.
// It returns per-field error messages; an empty map means valid.
func (t Template) Validate(raw map[string]string) (map[string]any, map[string]string) {
	vals, errs := map[string]any{}, map[string]string{}
	for _, in := range t.Inputs {
		v := strings.TrimSpace(raw[in.Name])
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
			if !slices.Contains(in.Options, v) {
				errs[in.Name] = "not an allowed option"
				continue
			}
			vals[in.Name] = v
		default:
			// anchored like the browser's pattern attribute: the whole value must match
			if in.Pattern != "" && !regexp.MustCompile("^(?:"+in.Pattern+")$").MatchString(v) {
				errs[in.Name] = "must match " + in.Pattern
				continue
			}
			vals[in.Name] = v
		}
	}
	return vals, errs
}
