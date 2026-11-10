// Package registry implements Opsolete's private Terraform module registry: archive
// inspection, address validation, signed download URLs and mapping module variables to form inputs.
// The HTTP protocol endpoints live in internal/http.
package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	tfaddr "github.com/hashicorp/terraform-registry-address"
	"github.com/zclconf/go-cty/cty"

	"github.com/opsolete/opsolete/internal/templates"
)

const (
	MaxArchiveSize   = 20 << 20  // compressed upload limit
	maxExtractedSize = 200 << 20 // guards against gzip bombs
	maxEntries       = 20000     // and against millions of empty files (inodes, CPU)
)

// Registry knows its public URL (whose host is the registry hostname) and the URL signing key.
type Registry struct {
	PublicURL *url.URL
	Key       []byte
}

// Address validates and normalises a module address with Terraform's own rules,
// returning the lowercase namespace, name and system plus the full source address.
func (r *Registry) Address(namespace, name, system string) (ns, n, sys, source string, err error) {
	raw := strings.ToLower(r.PublicURL.Host + "/" + namespace + "/" + name + "/" + system)
	m, err := tfaddr.ParseModuleSource(raw)
	if err != nil {
		return "", "", "", "", err
	}
	if m.Subdir != "" {
		return "", "", "", "", errors.New("namespace, name and system may not contain slashes")
	}
	p := m.Package
	return p.Namespace, p.Name, p.TargetSystem, p.String(), nil
}

// ParseSource splits a source address of this registry; ok is false for any other source (git, other registries).
func (r *Registry) ParseSource(source string) (ns, name, system string, ok bool) {
	m, err := tfaddr.ParseModuleSource(source)
	if err != nil || m.Subdir != "" || !strings.EqualFold(m.Package.Host.String(), r.PublicURL.Host) {
		return "", "", "", false
	}
	p := m.Package // registry addresses are case-insensitive; Address stores them lower-cased
	return strings.ToLower(p.Namespace), strings.ToLower(p.Name), strings.ToLower(p.TargetSystem), true
}

// CanonicalVersion parses a semver ("v1.2.0" or "1.2.0") into its canonical form.
func CanonicalVersion(v string) (string, error) {
	sv, err := version.NewSemver(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("version must be semver like 1.2.0: %w", err)
	}
	return sv.String(), nil
}

// SortVersions sorts semver strings newest first; invalid ones go last.
func SortVersions(vs []string) {
	slices.SortFunc(vs, func(a, b string) int {
		va, ea := version.NewSemver(a)
		vb, eb := version.NewSemver(b)
		if ea != nil || eb != nil {
			return strings.Compare(b, a)
		}
		return vb.Compare(va)
	})
}

// Newest returns the newest of vs matching a Terraform version constraint ("" any), as terraform init picks;
// false if none does or the constraint is invalid.
func Newest(vs []string, constraint string) (string, bool) {
	c, err := version.NewConstraint(constraint)
	if strings.TrimSpace(constraint) == "" {
		c, err = nil, nil
	}
	if err != nil {
		return "", false
	}
	vs = slices.Clone(vs)
	SortVersions(vs)
	for _, v := range vs {
		if sv, err := version.NewSemver(v); err == nil && (c == nil && sv.Prerelease() == "" || c != nil && c.Check(sv)) {
			return v, true
		}
	}
	return "", false
}

// ArchivePath is the path of a module archive. It ends in .tar.gz so Terraform's getter unpacks it.
func ArchivePath(ns, name, system, ver string) string {
	return fmt.Sprintf("/v1/archives/%s/%s/%s/%s.tar.gz", ns, name, system, ver)
}

func (r *Registry) sig(path string, exp int64) string {
	m := hmac.New(sha256.New, r.Key)
	fmt.Fprintf(m, "%s\n%d", path, exp)
	return hex.EncodeToString(m.Sum(nil))
}

// SignedArchiveURL returns path?expires=..&sig=.., valid for ttl. Terraform does not send
// registry credentials to download URLs, so the signature is what authorises the fetch.
func (r *Registry) SignedArchiveURL(path string, ttl time.Duration) string {
	exp := time.Now().Add(ttl).Unix()
	return fmt.Sprintf("%s?expires=%d&sig=%s", path, exp, r.sig(path, exp))
}

func (r *Registry) VerifyArchiveURL(path, expires, sig string) bool {
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(r.sig(path, exp)))
}

// RunnerSource resolves a template's module for `terraform init -from-module`: source?ref=ref, or for
// this registry's modules a relative signed archive path that runners resolve against the URL
// they reach Opsolete on (in k8s, the internal service).
func (r *Registry) RunnerSource(m templates.Module, ttl time.Duration) string {
	if ns, name, sys, ok := r.ParseSource(m.Source); ok {
		return r.SignedArchiveURL(ArchivePath(ns, name, sys, m.Ref), ttl)
	}
	sep := "?"
	if strings.Contains(m.Source, "?") { // e.g. git::https://host/repo//dir?depth=1
		sep = "&"
	}
	return m.Source + sep + "ref=" + url.QueryEscape(m.Ref)
}

// Inspection is what Inspect learns from a module archive.
type Inspection struct {
	Module    *tfconfig.Module // everything terraform-config-inspect reads: inputs, outputs, providers, resources...
	Variables []Variable       // Module.Variables in source order, for form generation
	Readme    string           // README.md at the archive root, if any
}

// Variable is a module variable plus the form pattern read from its validation blocks,
// which terraform-config-inspect doesn't read.
type Variable struct {
	tfconfig.Variable
	Pattern string `json:"pattern,omitempty"`
}

const maxReadme = 256 << 10

// Inspect unpacks a .tar.gz module archive into a temp dir and reads it with
// terraform-config-inspect. The module must be at the archive root.
func Inspect(archive []byte) (Inspection, error) {
	dir, err := os.MkdirTemp("", "ops-module-*")
	if err != nil {
		return Inspection{}, err
	}
	defer os.RemoveAll(dir)
	if err := extract(archive, dir); err != nil {
		return Inspection{}, err
	}
	if !tfconfig.IsModuleDir(dir) {
		return Inspection{}, errors.New("no .tf files at the archive root (create it with: tar czf module.tar.gz -C <module-dir> .)")
	}
	mod, diags := tfconfig.LoadModule(dir)
	if diags.HasErrors() {
		return Inspection{}, fmt.Errorf("invalid module: %w", diags.Err())
	}
	// Don't persist temp paths; keep file names relative to the module root.
	mod.Path, mod.Diagnostics = "", nil
	rel := func(p *tfconfig.SourcePos) { p.Filename = filepath.Base(p.Filename) }
	for _, v := range mod.Variables {
		rel(&v.Pos)
	}
	for _, o := range mod.Outputs {
		rel(&o.Pos)
	}
	for _, r := range mod.ManagedResources {
		rel(&r.Pos)
	}
	for _, r := range mod.DataResources {
		rel(&r.Pos)
	}
	for _, c := range mod.ModuleCalls {
		rel(&c.Pos)
	}

	patterns := regexPatterns(dir)
	vars := make([]Variable, 0, len(mod.Variables))
	for _, v := range mod.Variables {
		vars = append(vars, Variable{Variable: *v, Pattern: patterns[v.Name]})
	}
	// File order, so the generated form follows the module author's ordering.
	slices.SortFunc(vars, func(a, b Variable) int {
		if c := strings.Compare(a.Pos.Filename, b.Pos.Filename); c != 0 {
			return c
		}
		return a.Pos.Line - b.Pos.Line
	})

	var readme string
	if f, err := os.Open(filepath.Join(dir, "README.md")); err == nil {
		b, _ := io.ReadAll(io.LimitReader(f, maxReadme))
		f.Close()
		readme = string(b)
	}
	return Inspection{Module: mod, Variables: vars, Readme: readme}, nil
}

// regexPatterns returns, by variable name, form patterns from validation conditions written as
// can(regex("<literal>", var.<the variable>)).
// shortcut: other conditions (length, contains, &&, ...), .tf.json files and all but the first regex per
// variable are skipped; Terraform still checks them at plan time. Read more forms when modules need it.
func regexPatterns(dir string) map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		f, diags := hclsyntax.ParseConfig(src, name, hcl.InitialPos)
		if diags.HasErrors() {
			continue
		}
		for _, b := range f.Body.(*hclsyntax.Body).Blocks {
			if b.Type != "variable" || len(b.Labels) != 1 || out[b.Labels[0]] != "" {
				continue
			}
			for _, v := range b.Body.Blocks {
				if a, ok := v.Body.Attributes["condition"]; ok && v.Type == "validation" {
					if p, ok := regexCondition(a.Expr, b.Labels[0]); ok {
						out[b.Labels[0]] = p
						break
					}
				}
			}
		}
	}
	return out
}

// regexCondition matches can(regex("<literal>", var.<name>)) and returns the regex as a form pattern.
func regexCondition(e hclsyntax.Expression, name string) (string, bool) {
	can, ok := e.(*hclsyntax.FunctionCallExpr)
	if !ok || can.Name != "can" || len(can.Args) != 1 {
		return "", false
	}
	re, ok := can.Args[0].(*hclsyntax.FunctionCallExpr)
	if !ok || re.Name != "regex" || len(re.Args) != 2 {
		return "", false
	}
	t, diags := hcl.AbsTraversalForExpr(re.Args[1])
	if diags.HasErrors() || len(t) != 2 || t.RootName() != "var" {
		return "", false
	}
	if a, ok := t[1].(hcl.TraverseAttr); !ok || a.Name != name {
		return "", false
	}
	v, diags := re.Args[0].Value(nil) // nil context: interpolations and function calls fail, so only literals pass
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || !v.Type().Equals(cty.String) {
		return "", false
	}
	return formPattern(v.AsString())
}

// formPattern turns a Terraform regex, which matches anywhere in the value, into a form pattern,
// which must match the whole value. Patterns anchored at both ends are kept as written.
// shortcut: the .* wrapper doesn't cross newlines; fine for single-line form fields.
func formPattern(re string) (string, bool) {
	s, err := syntax.Parse(re, syntax.Perl) // the flags regexp.Compile, and so Terraform's regex(), use
	if err != nil {
		return "", false
	}
	if s.Op == syntax.OpConcat && len(s.Sub) >= 2 && s.Sub[0].Op == syntax.OpBeginText && s.Sub[len(s.Sub)-1].Op == syntax.OpEndText {
		return re, true
	}
	return ".*(?:" + re + ").*", true
}

// extract unpacks regular files and directories only, rejecting paths that escape dir.
func extract(archive []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("not a .tar.gz archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if entries >= maxEntries {
			return fmt.Errorf("archive has more than %d entries", maxEntries)
		}
		name := filepath.Clean(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("archive entry %q escapes the module directory", h.Name)
		}
		target := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxExtractedSize-total+1))
			total += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			if total > maxExtractedSize {
				return errors.New("archive is too large when unpacked")
			}
		}
		// Symlinks and other entry types are skipped on purpose.
	}
}

// Inputs maps module variables to form inputs. A list(object({...})) of strings, numbers and bools is a list input
// (rows of fields), list(string) and set(string) a string_list, map(string) a map, object({...}) of strings, numbers
// and bools an object; other complex types (lists of lists, maps of objects, ...) can't be a form field yet: they
// are returned in skipped so the builder can warn about them.
func Inputs(vars []Variable) (inputs []templates.Input, skipped []string) {
	for _, v := range vars {
		in := templates.Input{Name: v.Name, Required: v.Required}
		// descriptions often read "VM name: 1-64 letters…": the label, then help for the form
		in.Label, in.Help, _ = strings.Cut(v.Description, ": ")
		switch v.Type {
		case "", "string":
			in.Type = "string"
		case "number":
			in.Type = "number"
		case "bool":
			in.Type = "bool"
		default:
			typ, fields, ok := complexType(v.Type)
			if !ok {
				skipped = append(skipped, fmt.Sprintf("%s (%s)", v.Name, v.Type))
				continue
			}
			in.Type, in.Fields = typ, fields
		}
		if in.Type == "string" {
			in.Pattern = v.Pattern
		}
		if in.Label == "" {
			in.Label = v.Name
		}
		if v.Default != nil {
			in.Default = templates.FormatValue(v.Default)
		}
		inputs = append(inputs, in)
	}
	return inputs, skipped
}

// complexType reads a variable's complex type as an input type: list(object({...})) a list and object({...}) an
// object, both of string, number and bool attributes (fields: objectFields), list(string) and set(string) a
// string_list, map(string) a map.
func complexType(src string) (typ string, fields []templates.Input, ok bool) {
	expr, diags := hclsyntax.ParseExpression([]byte(src), "type", hcl.InitialPos)
	if diags.HasErrors() {
		return "", nil, false
	}
	ty, defs, diags := typeexpr.TypeConstraintWithDefaults(expr)
	switch {
	case diags.HasErrors():
		return "", nil, false
	case ty.Equals(cty.List(cty.String)) || ty.Equals(cty.Set(cty.String)):
		return "string_list", nil, true
	case ty.Equals(cty.Map(cty.String)):
		return "map", nil, true
	case ty.IsObjectType():
		fields, ok := objectFields(expr, ty, defs)
		return "object", fields, ok
	case ty.IsListType() && ty.ElementType().IsObjectType():
		call, _ := expr.(*hclsyntax.FunctionCallExpr) // list(object({ … }))
		if call == nil || len(call.Args) != 1 {
			return "", nil, false
		}
		var elDefs *typeexpr.Defaults
		if defs != nil {
			elDefs = defs.Children[""]
		}
		fields, ok := objectFields(call.Args[0], ty.ElementType(), elDefs)
		return "list", fields, ok
	}
	return "", nil, false
}

// objectFields reads an object({...}) type of string, number and bool attributes as fields, in the order written
// (expr): optional(...) ones aren't required, and optional(t, d)'s d is the field's default.
func objectFields(expr hclsyntax.Expression, ty cty.Type, defs *typeexpr.Defaults) ([]templates.Input, bool) {
	obj, _ := expr.(*hclsyntax.FunctionCallExpr)
	if obj == nil || len(obj.Args) != 1 {
		return nil, false
	}
	attrs, _ := obj.Args[0].(*hclsyntax.ObjectConsExpr)
	if attrs == nil {
		return nil, false
	}
	var fields []templates.Input
	for _, item := range attrs.Items {
		name := hcl.ExprAsKeyword(item.KeyExpr)
		if !ty.HasAttribute(name) {
			return nil, false
		}
		f := templates.Input{Name: name, Label: name, Required: !ty.AttributeOptional(name)}
		var d cty.Value
		if defs != nil {
			d = defs.DefaultValues[name]
		}
		known := d != cty.NilVal && d.IsKnown() && !d.IsNull()
		switch ty.AttributeType(name) {
		case cty.String:
			f.Type = "string"
			if known {
				f.Default = d.AsString()
			}
		case cty.Number:
			f.Type = "number"
			if known {
				n, _ := d.AsBigFloat().Float64()
				f.Default = templates.FormatValue(n)
			}
		case cty.Bool:
			f.Type = "bool"
			if known {
				f.Default = strconv.FormatBool(d.True())
			}
		default:
			return nil, false
		}
		fields = append(fields, f)
	}
	return fields, len(fields) > 0
}

// overrideField applies what a template's JSON adds to a generated input or field: a label, help, a default, a
// pattern (replacing the module's), select options (over a string).
func overrideField(in, o templates.Input) templates.Input {
	if o.Type == "select" && in.Type == "string" {
		in.Type, in.Options = "select", o.Options
	}
	if o.Label != "" {
		in.Label = o.Label
	}
	if o.Help != "" {
		in.Help = o.Help
	}
	if o.Default != "" {
		in.Default = o.Default
	}
	if o.Pattern != "" {
		in.Pattern = o.Pattern
	}
	in.Required = in.Required || o.Required
	return in
}

// Resolve makes a template's form match the module version it pins: one input per module variable,
// with name, type, required-ness and regex validation pattern from variables.tf. The template's JSON
// inputs only add a label, help, a pattern (replacing the module's), select options or default, and keep their order;
// a list's fields the same (by name; the module's type says which there are). JSON inputs the module doesn't declare are
// dropped. Sensitive variables appear only if the JSON lists them; listed, they are sensitive inputs.
func Resolve(overrides []templates.Input, vars []Variable) []templates.Input {
	gen, _ := Inputs(vars)
	sensitive := map[string]bool{}
	for _, v := range vars {
		sensitive[v.Name] = v.Sensitive
	}
	byName := map[string]templates.Input{}
	for _, in := range gen {
		byName[in.Name] = in
	}
	var out []templates.Input
	for _, o := range overrides {
		in, ok := byName[o.Name]
		if !ok {
			continue
		}
		delete(byName, o.Name)
		in = overrideField(in, o)
		in.ShowIf = o.ShowIf
		for i, f := range in.Fields {
			if j := slices.IndexFunc(o.Fields, func(of templates.Input) bool { return of.Name == f.Name }); j >= 0 {
				in.Fields[i] = overrideField(f, o.Fields[j])
			}
		}
		in.Sensitive = o.Sensitive || sensitive[o.Name] && (in.Type == "string" || in.Type == "number")
		if in.Sensitive {
			in.Default = "" // never echoed into the form
		}
		in.Key = o.Key && !in.Sensitive && in.Type != "bool"
		in.PerEnvironment = o.PerEnvironment && !in.Sensitive // a module's sensitive variable stays sealed per workspace
		out = append(out, in)
	}
	for _, v := range vars {
		if in, ok := byName[v.Name]; ok && !v.Sensitive {
			out = append(out, in)
		}
	}
	return out
}
