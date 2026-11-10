package registry

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// What a scan can find for a template, read from its module's code (a workspace's Import), whatever the variables
// are called. The module tree is followed: local module calls (./x) and modules of this registry (fetch), values
// through call arguments, locals and outputs. Every managed resource is an entry, found with its own type as list
// resource; the code says the rest:
//   - count = var.x == "v" ? 1 : 0 (or var.x ? 1 : 0), on the resource or its module call: it exists for x = v
//   - attribute = var.x, coalesce(var.x, …) or try(var.x, …), nested blocks too: input x = the attribute's path
//   - dynamic "b" { for_each = var.x ? … : [] }: the yes / no input x = whether b is set
//   - attribute = [other.id] or other.id (also through outputs): other is related, found from this attribute
//   - a sensitive input in an attribute in lifecycle ignore_changes: a placeholder
//
// Optional and repeated parts (count = var.x != null ? 1 : 0, or for_each over a list input's rows, on a module
// call) aren't searched: Terraform can't list most of them (data disks, schedules). When their module imports what
// exists itself (import blocks switched by an input, e.g. existing = true) they're probes (probe): the runner tries
// the inventory's resources with that module, and fills the form from what imports. Otherwise the form asks them.
// shortcut: other expressions, for_each on resources and other sources are skipped too (listed in the notes); the
// form asks them when importing.

// requireByType is for list resources that also return resources of another kind: the attribute only theirs has.
var requireByType = map[string]string{
	"azurerm_windows_virtual_machine": "provision_vm_agent",              // the Linux list returns Windows VMs too
	"azurerm_linux_virtual_machine":   "disable_password_authentication", // and the other way round
}

// Fetch returns the archive of a module of this registry: its source address and version constraint. nil: only
// local module calls are followed.
type Fetch func(source, version string) ([]byte, error)

// maxDepth bounds module nesting, against a module calling itself through the registry.
const maxDepth = 8

// module is a module's code: its resources, module calls, locals and outputs.
type module struct {
	resources []*hclsyntax.Block
	calls     []*hclsyntax.Block
	imports   []*hclsyntax.Block
	vars      map[string]bool // variable → whether it has a default
	locals    map[string]hclsyntax.Expression
	outputs   map[string]hclsyntax.Expression
	dir       string // where its local calls start, in its archive
	archive   string // the archive's root on disk
}

// inst is a module instance: its module, address prefix, and the call that made it.
type inst struct {
	mod    *module
	prefix string                          // e.g. module.windows[0].
	args   map[string]hclsyntax.Expression // the call's arguments, in parent's scope
	parent *inst
	showIf *templates.ShowIf // from the call's count
}

// Adoptable reads what a scan can find for a module (a registry archive) used with a template's inputs, and notes
// on what it skipped.
func Adoptable(archive []byte, inputs []templates.Input, fetch Fetch) ([]runnerapi.ScanEntry, []runnerapi.Probe, []string, error) {
	r := reader{inputs: inputs, fetch: fetch}
	defer r.cleanup()
	root, err := r.load(archive)
	if err != nil {
		return nil, nil, nil, err
	}
	r.walk(&inst{mod: root}, 0)
	r.classify()
	return r.entries, r.probes, r.notes, nil
}

type reader struct {
	inputs  []templates.Input
	fetch   Fetch
	dirs    []string // extracted archives, removed by cleanup
	entries []runnerapi.ScanEntry
	refs    [][]reference // by entry: the resources its attributes refer to
	probes  []runnerapi.Probe
	notes   []string
}

type reference struct{ to, path string }

func (r *reader) cleanup() {
	for _, d := range r.dirs {
		os.RemoveAll(d)
	}
}

// load extracts an archive and reads the module at its root.
func (r *reader) load(archive []byte) (*module, error) {
	dir, err := os.MkdirTemp("", "ops-adopt-*")
	if err != nil {
		return nil, err
	}
	r.dirs = append(r.dirs, dir)
	if err := extract(archive, dir); err != nil {
		return nil, err
	}
	return readModule(dir, dir)
}

func readModule(archive, dir string) (*module, error) {
	m := &module{locals: map[string]hclsyntax.Expression{}, outputs: map[string]hclsyntax.Expression{}, vars: map[string]bool{}, dir: dir, archive: archive}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	slices.Sort(files)
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		f, diags := hclsyntax.ParseConfig(src, filepath.Base(name), hcl.InitialPos)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: %w", filepath.Base(name), diags)
		}
		for _, b := range f.Body.(*hclsyntax.Body).Blocks {
			switch {
			case b.Type == "resource" && len(b.Labels) == 2:
				m.resources = append(m.resources, b)
			case b.Type == "module" && len(b.Labels) == 1:
				m.calls = append(m.calls, b)
			case b.Type == "import":
				m.imports = append(m.imports, b)
			case b.Type == "variable" && len(b.Labels) == 1:
				m.vars[b.Labels[0]] = b.Body.Attributes["default"] != nil
			case b.Type == "locals":
				for k, a := range b.Body.Attributes {
					m.locals[k] = a.Expr
				}
			case b.Type == "output" && len(b.Labels) == 1:
				if a, ok := b.Body.Attributes["value"]; ok {
					m.outputs[b.Labels[0]] = a.Expr
				}
			}
		}
	}
	return m, nil
}

// child reads the module a call loads: a local path in the same archive, or one of this registry's.
func (r *reader) child(in *inst, call *hclsyntax.Block) (*module, error) {
	src := literal(attrExpr(call.Body, "source"))
	if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
		dir := filepath.Join(in.mod.dir, filepath.FromSlash(src))
		if rel, err := filepath.Rel(in.mod.archive, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s leaves the module's archive", src)
		}
		return readModule(in.mod.archive, dir)
	}
	if r.fetch == nil {
		return nil, fmt.Errorf("source %s isn't followed", src)
	}
	archive, err := r.fetch(src, literal(attrExpr(call.Body, "version")))
	if err != nil {
		return nil, err
	}
	return r.load(archive)
}

func attrExpr(b *hclsyntax.Body, name string) hclsyntax.Expression {
	if a, ok := b.Attributes[name]; ok {
		return a.Expr
	}
	return nil
}

// walk makes entries of in's resources, then of its module calls' instances.
func (r *reader) walk(in *inst, depth int) {
	for _, b := range in.mod.resources {
		r.resource(in, b)
	}
	for _, call := range in.mod.calls {
		name := in.prefix + "module." + call.Labels[0]
		if depth >= maxDepth {
			r.notes = append(r.notes, name+": modules nested too deep, skipped")
			continue
		}
		mod, err := r.child(in, call)
		if err != nil {
			r.notes = append(r.notes, name+": "+err.Error()+", skipped")
			continue
		}
		c := &inst{mod: mod, parent: in, args: map[string]hclsyntax.Expression{}, showIf: in.showIf}
		for k, a := range call.Body.Attributes {
			c.args[k] = a.Expr
		}
		switch {
		case c.args["for_each"] != nil:
			list, key := r.repeatOf(in, c.args["for_each"])
			r.probe(in, call, c, name, list, key)
			continue
		case c.args["count"] != nil:
			ok, optional := r.count(in, c.args["count"], &c.showIf)
			if optional {
				r.probe(in, call, c, name, "", "")
				continue
			}
			if !ok {
				r.notes = append(r.notes, name+": its count isn't var.x == \"value\" ? 1 : 0, skipped")
				continue
			}
			c.prefix = name + "[0]."
		default:
			c.prefix = name + "."
		}
		r.walk(c, depth+1)
	}
}

// count reads count = var.x == "v" ? 1 : 0 (either side of ==) or var.x ? 1 : 0 as show_if (ok), and
// var.x != null ? 1 : 0 as optional.
func (r *reader) count(in *inst, e hclsyntax.Expression, showIf **templates.ShowIf) (ok, optional bool) {
	c, isCond := e.(*hclsyntax.ConditionalExpr)
	if !isCond || !isNumber(c.TrueResult, 1) || !isNumber(c.FalseResult, 0) {
		return false, false
	}
	if v := r.resolve(in, c.Condition); v.input != "" {
		if in := r.input(v.input); in != nil && in.Type == "bool" {
			*showIf = &templates.ShowIf{Input: in.Name, Equals: []string{"true"}}
			return true, false
		}
	}
	op, isOp := c.Condition.(*hclsyntax.BinaryOpExpr)
	if !isOp {
		return false, false
	}
	v, lit := r.resolve(in, op.LHS), op.RHS
	if v.input == "" {
		v, lit = r.resolve(in, op.RHS), op.LHS
	}
	if v.input == "" || r.input(v.input) == nil {
		return false, false
	}
	switch {
	case op.Op == hclsyntax.OpNotEqual && isNull(lit):
		return false, true
	case op.Op == hclsyntax.OpEqual && literal(lit) != "":
		*showIf = &templates.ShowIf{Input: v.input, Equals: []string{literal(lit)}}
		return true, false
	}
	return false, false
}

// val is what an expression comes from: a root input, a field of a repeated module call's row (each.value.x), or
// the IDs of resources (as a one-element list: tuple).
type val struct {
	input, field string
	refs         []string
	tuple        bool
}

// resolve follows an expression in in's scope to what it comes from.
func (r *reader) resolve(in *inst, e hclsyntax.Expression) val {
	switch x := e.(type) {
	case *hclsyntax.FunctionCallExpr:
		switch {
		case (x.Name == "coalesce" || x.Name == "try") && len(x.Args) > 0:
			v := r.resolve(in, x.Args[0])
			for _, a := range x.Args[1:] { // coalesce(one(a[*].id), one(b[*].id)): either's id
				v.refs = append(v.refs, r.resolve(in, a).refs...)
			}
			return v
		case (x.Name == "one" || x.Name == "tostring") && len(x.Args) == 1:
			return r.resolve(in, x.Args[0])
		}
	case *hclsyntax.TupleConsExpr:
		if len(x.Exprs) == 1 {
			v := r.resolve(in, x.Exprs[0])
			v.tuple = true
			return v
		}
	case *hclsyntax.SplatExpr: // module.m[*].out
		if t, diags := hcl.AbsTraversalForExpr(x.Source); !diags.HasErrors() && t.RootName() == "module" && len(t) == 2 {
			if each, ok := x.Each.(*hclsyntax.RelativeTraversalExpr); ok && len(each.Traversal) == 1 {
				if out, ok := each.Traversal[0].(hcl.TraverseAttr); ok {
					call, _ := t[1].(hcl.TraverseAttr)
					return r.output(in, call.Name, out.Name)
				}
			}
		}
	case *hclsyntax.ParenthesesExpr:
		return r.resolve(in, x.Expression)
	}
	t, diags := hcl.AbsTraversalForExpr(e)
	if diags.HasErrors() || len(t) < 2 {
		return val{}
	}
	attr := func(i int) string {
		if i < len(t) {
			if a, ok := t[i].(hcl.TraverseAttr); ok {
				return a.Name
			}
		}
		return ""
	}
	switch t.RootName() {
	case "var":
		if in.parent == nil {
			return val{input: attr(1)}
		}
		if a := in.args[attr(1)]; a != nil {
			return r.resolve(in.parent, a)
		}
	case "local":
		if l := in.mod.locals[attr(1)]; l != nil {
			return r.resolve(in, l)
		}
	case "module":
		out := attr(2)
		if _, ok := t[2].(hcl.TraverseIndex); ok { // module.m[0].out
			out = attr(3)
		}
		return r.output(in, attr(1), out)
	case "each":
		if attr(1) == "value" && attr(2) != "" {
			return val{field: attr(2)}
		}
	case "data", "path", "count", "terraform", "self":
	default: // type.name.id, type.name[0].id
		name, id := attr(1), attr(len(t)-1)
		if name != "" && id == "id" && slices.ContainsFunc(in.mod.resources, func(b *hclsyntax.Block) bool {
			return b.Labels[0] == t.RootName() && b.Labels[1] == name
		}) {
			return val{refs: []string{in.prefix + t.RootName() + "." + name}}
		}
	}
	return val{}
}

// output resolves output out of in's module call name, in the called module's scope.
func (r *reader) output(in *inst, name, out string) val {
	i := slices.IndexFunc(in.mod.calls, func(b *hclsyntax.Block) bool { return b.Labels[0] == name })
	if i < 0 {
		return val{}
	}
	call := in.mod.calls[i]
	mod, err := r.child(in, call)
	if err != nil || mod.outputs[out] == nil {
		return val{}
	}
	c := &inst{mod: mod, parent: in, args: map[string]hclsyntax.Expression{}, prefix: in.prefix + "module." + name + "."}
	for k, a := range call.Body.Attributes {
		c.args[k] = a.Expr
	}
	if c.args["count"] != nil {
		c.prefix = in.prefix + "module." + name + "[0]."
	}
	v := r.resolve(c, mod.outputs[out])
	v.input, v.field = "", "" // an output's value is only followed for resource IDs
	return v
}

func (r *reader) input(name string) *templates.Input {
	i := slices.IndexFunc(r.inputs, func(in templates.Input) bool { return in.Name == name })
	if i < 0 {
		return nil
	}
	return &r.inputs[i]
}

// resource adds the entry of a resource of in.
func (r *reader) resource(in *inst, b *hclsyntax.Block) {
	addr := in.prefix + b.Labels[0] + "." + b.Labels[1]
	if attrExpr(b.Body, "for_each") != nil {
		r.notes = append(r.notes, addr+": for_each on a resource isn't supported, skipped")
		return
	}
	e := runnerapi.ScanEntry{To: addr, List: b.Labels[0], Require: requireByType[b.Labels[0]], ShowIf: in.showIf, Inputs: map[string]string{}}
	if c := attrExpr(b.Body, "count"); c != nil {
		ok, optional := r.count(in, c, &e.ShowIf)
		if optional {
			r.notes = append(r.notes, addr+": optional, filled in on the form")
			return
		}
		if !ok {
			r.notes = append(r.notes, addr+": its count isn't var.x == \"value\" ? 1 : 0, skipped")
			return
		}
		e.To += "[0]"
	}
	ignored := map[string]bool{}
	for _, blk := range b.Body.Blocks {
		if blk.Type == "lifecycle" {
			if ic := attrExpr(blk.Body, "ignore_changes"); ic != nil {
				list, _ := hcl.ExprList(ic)
				for _, x := range list {
					if t, diags := hcl.AbsTraversalForExpr(x); !diags.HasErrors() {
						ignored[t.RootName()] = true
					}
				}
			}
		}
	}
	n := len(r.entries)
	r.entries = append(r.entries, e)
	r.body(in, b.Body, "", n, ignored)
	if e := &r.entries[n]; len(e.Inputs) == 0 {
		e.Inputs = nil
	}
}

// body maps the attributes and blocks of b (at path prefix) into entry n.
func (r *reader) body(in *inst, b *hclsyntax.Body, prefix string, n int, ignored map[string]bool) {
	e := &r.entries[n]
	names := make([]string, 0, len(b.Attributes))
	for k := range b.Attributes {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, name := range names {
		if prefix == "" && slices.Contains([]string{"count", "for_each", "provider", "depends_on"}, name) {
			continue
		}
		p := prefix + name
		v := r.resolve(in, b.Attributes[name].Expr)
		for _, ref := range v.refs {
			if v.tuple {
				r.refer(n, ref, p+"[0]")
			} else {
				r.refer(n, ref, p)
			}
		}
		switch {
		case v.input != "":
			ti := r.input(v.input)
			if ti == nil {
				continue
			}
			if ti.Sensitive {
				if prefix == "" && ignored[name] && !slices.Contains(e.Placeholder, v.input) {
					e.Placeholder = append(e.Placeholder, v.input)
				}
				continue // a sensitive value never fills the form
			}
			if _, ok := e.Inputs[v.input]; !ok {
				e.Inputs[v.input] = p
			}
		}
	}
	for _, blk := range b.Blocks {
		switch {
		case blk.Type == "lifecycle" || blk.Type == "timeouts" || blk.Type == "provisioner" || blk.Type == "connection":
		case blk.Type == "dynamic" && len(blk.Labels) == 1:
			r.dynamic(in, blk, prefix, n)
		default:
			r.body(in, blk.Body, prefix+blk.Type+"[0].", n, ignored)
		}
	}
}

// repeatOf reads a module call's for_each = { for d in var.x : tostring(d.k) => d } (through locals) as rows of the
// list input x keyed by its field k; "" if it isn't one.
func (r *reader) repeatOf(in *inst, e hclsyntax.Expression) (list, key string) {
	if t, diags := hcl.AbsTraversalForExpr(e); !diags.HasErrors() && t.RootName() == "local" && len(t) == 2 {
		if a, ok := t[1].(hcl.TraverseAttr); ok && in.mod.locals[a.Name] != nil {
			return r.repeatOf(in, in.mod.locals[a.Name])
		}
	}
	f, ok := e.(*hclsyntax.ForExpr)
	if !ok || f.KeyExpr == nil || f.CondExpr != nil {
		return "", ""
	}
	k := f.KeyExpr
	if fc, ok := k.(*hclsyntax.FunctionCallExpr); ok && fc.Name == "tostring" && len(fc.Args) == 1 {
		k = fc.Args[0]
	}
	t, diags := hcl.AbsTraversalForExpr(k)
	if diags.HasErrors() || len(t) != 2 || t.RootName() != f.ValVar {
		return "", ""
	}
	field, _ := t[1].(hcl.TraverseAttr)
	list = r.resolve(in, f.CollExpr).input
	if ti := r.input(list); ti == nil || ti.Type != "list" {
		return "", ""
	}
	return list, field.Name
}

// probeIDRe finds the type an import ID names: /providers/<namespace>/<type>/ right before the resource's name.
var probeIDRe = regexp.MustCompile(`/providers/([^/]+/[^/]+)/$`)

// probe makes a Probe of an optional or repeated module call (c, its instance; list and key: the rows it's
// repeated over), or notes why the form asks it instead.
func (r *reader) probe(in *inst, call *hclsyntax.Block, c *inst, name, list, key string) {
	note := func(why string) { r.notes = append(r.notes, name+": "+why+", filled in on the form") }
	repeated := c.args["for_each"] != nil
	src := literal(attrExpr(call.Body, "source"))
	switch {
	case repeated && list == "":
		note("repeated, but not over a list input's rows ({ for d in var.x : tostring(d.k) => d })")
		return
	case strings.HasPrefix(src, "."):
		note("a local module, not tried")
		return
	case len(c.mod.imports) == 0:
		note("optional; its module imports nothing")
		return
	}
	p := runnerapi.Probe{Call: name, Source: src, Version: literal(attrExpr(call.Body, "version")), List: list, Key: key,
		Imports: len(c.mod.imports), Args: map[string]runnerapi.ProbeArg{}}
	var switchVar, nameVar string
	for _, im := range c.mod.imports {
		if cond, ok := attrExpr(im.Body, "for_each").(*hclsyntax.ConditionalExpr); ok { // var.existing ? toset([…]) : toset([])
			if t, diags := hcl.AbsTraversalForExpr(cond.Condition); !diags.HasErrors() && t.RootName() == "var" && len(t) == 2 {
				a, _ := t[1].(hcl.TraverseAttr)
				switchVar = a.Name
			}
		}
		if typ, v := idType(c.mod, attrExpr(im.Body, "id")); typ != "" && p.Type == "" {
			p.Type, nameVar = typ, v
		}
	}
	if switchVar == "" {
		note("its module's import blocks aren't switched by an input (for_each = var.x ? … : [])")
		return
	}
	for arg, e := range c.args {
		if slices.Contains([]string{"source", "version", "count", "for_each", "providers", "depends_on"}, arg) {
			continue
		}
		v := r.resolve(in, e)
		a := runnerapi.ProbeArg{Input: v.input, Field: v.field, Refs: v.refs, Switch: arg == switchVar, Name: arg == nameVar}
		if a.Input == "" && a.Field == "" && len(a.Refs) == 0 && !a.Switch && !a.Name {
			if !c.mod.vars[arg] {
				note("its argument " + arg + " comes from neither an input nor a found resource")
				return
			}
			continue // the module's default
		}
		p.Args[arg] = a
	}
	if _, ok := p.Args[switchVar]; !ok {
		note("it doesn't pass its module's import switch " + switchVar)
		return
	}
	for _, b := range c.mod.resources {
		f := runnerapi.ProbeFill{To: b.Labels[0] + "." + b.Labels[1], Values: map[string]string{}}
		r.fill(c, b.Body, "", &f)
		p.Fills = append(p.Fills, f)
	}
	r.probes = append(r.probes, p)
}

// idType reads an import ID ("${…}/providers/Microsoft.Compute/disks/${var.name}", also through a local) for the
// type it names and the variable of the name after it; "" if it names none.
func idType(m *module, e hclsyntax.Expression) (typ, nameVar string) {
	if t, diags := hcl.AbsTraversalForExpr(e); !diags.HasErrors() && t.RootName() == "local" && len(t) == 2 {
		if a, ok := t[1].(hcl.TraverseAttr); ok {
			e = m.locals[a.Name]
		}
	}
	tmpl, ok := e.(*hclsyntax.TemplateExpr)
	if !ok {
		return "", ""
	}
	var lit string
	for _, part := range tmpl.Parts {
		if s := literal(part); s != "" {
			lit += s
			continue
		}
		t, diags := hcl.AbsTraversalForExpr(part)
		if m := probeIDRe.FindStringSubmatch(lit); m != nil && !diags.HasErrors() && t.RootName() == "var" && len(t) == 2 {
			a, _ := t[1].(hcl.TraverseAttr)
			return m[1], a.Name
		}
		lit = ""
	}
	return "", ""
}

// fill maps the attributes of a probed module's resource (body, at path prefix) back to the form: an input or
// row field each, and a dynamic block over a list input to that list's rows.
func (r *reader) fill(c *inst, b *hclsyntax.Body, prefix string, f *runnerapi.ProbeFill) {
	for _, name := range slices.Sorted(maps.Keys(b.Attributes)) {
		v := r.resolve(c, b.Attributes[name].Expr)
		if k := cmp.Or(v.input, v.field); k != "" && f.Values[k] == "" {
			if ti := r.input(v.input); v.field != "" || ti != nil && !ti.Sensitive {
				f.Values[k] = prefix + name
			}
		}
	}
	for _, blk := range b.Blocks {
		switch {
		case blk.Type == "lifecycle" || blk.Type == "timeouts":
		case blk.Type == "dynamic" && len(blk.Labels) == 1:
			list := r.resolve(c, attrExpr(blk.Body, "for_each")).input
			if ti := r.input(list); ti == nil || ti.Type != "list" {
				continue
			}
			bl := runnerapi.BlockList{Block: prefix + blk.Labels[0], Fields: map[string]string{}}
			for _, content := range blk.Body.Blocks {
				for attr, a := range content.Body.Attributes {
					if t, diags := hcl.AbsTraversalForExpr(a.Expr); !diags.HasErrors() && len(t) == 3 && t.RootName() == blk.Labels[0] {
						if field, ok := t[2].(hcl.TraverseAttr); ok {
							bl.Fields[field.Name] = attr
						}
					}
				}
			}
			if f.Lists == nil {
				f.Lists = map[string]runnerapi.BlockList{}
			}
			f.Lists[list] = bl
		default:
			r.fill(c, blk.Body, prefix+blk.Type+"[0].", f)
		}
	}
}

// dynamic maps a dynamic block with for_each = var.x ? … : []: the yes / no input x says whether it's set.
func (r *reader) dynamic(in *inst, blk *hclsyntax.Block, prefix string, n int) {
	if c, ok := attrExpr(blk.Body, "for_each").(*hclsyntax.ConditionalExpr); ok {
		if v := r.resolve(in, c.Condition); v.input != "" {
			if ti := r.input(v.input); ti != nil && ti.Type == "bool" {
				r.entries[n].Inputs[v.input] = prefix + blk.Labels[0]
			}
		}
	}
}

func (r *reader) refer(n int, to, path string) {
	for len(r.refs) <= n {
		r.refs = append(r.refs, nil)
	}
	if to != r.entries[n].To && strings.TrimSuffix(to, "[0]") != strings.TrimSuffix(r.entries[n].To, "[0]") {
		r.refs[n] = append(r.refs[n], reference{to, path})
	}
}

// classify relates the entries: a resource another one refers to is related (IDFrom: the referring attribute).
func (r *reader) classify() {
	for n, refs := range r.refs {
		for _, ref := range refs {
			i := slices.IndexFunc(r.entries, func(e runnerapi.ScanEntry) bool {
				return e.To == ref.to || strings.TrimSuffix(e.To, "[0]") == ref.to
			})
			if i >= 0 && i != n && r.entries[i].IDFrom == "" {
				r.entries[i].IDFrom = ref.path
			}
		}
	}
}

func literal(e hclsyntax.Expression) string {
	if e == nil {
		return ""
	}
	v, diags := e.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || !v.Type().Equals(cty.String) {
		return ""
	}
	return v.AsString()
}

func isNull(e hclsyntax.Expression) bool {
	v, diags := e.Value(nil)
	return !diags.HasErrors() && v.IsNull()
}

func isNumber(e hclsyntax.Expression, n int64) bool {
	v, diags := e.Value(nil)
	return !diags.HasErrors() && v.IsKnown() && !v.IsNull() && v.Type().Equals(cty.Number) && v.Equals(cty.NumberIntVal(n)).True()
}
