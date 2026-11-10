package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// What a scan can find for a template, read from its module's code (a workspace's Import): every managed
// resource of the module's root is an entry, found with its own type as list resource, and the module's code
// says the rest, whatever the variables are called:
//   - count = var.x == "v" ? 1 : 0 (or var.x ? 1 : 0): it exists for x = v, address [0]
//   - attribute = var.x, or coalesce(var.x, ...): inputs x = the attribute's path (nested blocks: block[0].attr)
//   - dynamic "b" { for_each = var.x ? [...] : [] }: the yes / no input x = whether b is set
//   - attribute = [other.res.id] or other.res.id: other is a related entry, id_from that path
//   - a sensitive input in an attribute in lifecycle ignore_changes: a placeholder
// shortcut: other expressions, for_each, nested modules and .tf.json files are skipped (listed in the notes);
// the form asks them when importing. Read more forms when modules need it.

// requireByType is for list resources that also return resources of another kind: the attribute only theirs has.
var requireByType = map[string]string{
	"azurerm_windows_virtual_machine": "provision_vm_agent",              // the Linux list returns Windows VMs too
	"azurerm_linux_virtual_machine":   "disable_password_authentication", // and the other way round
}

// resourceBlock is a managed resource of the module, as the generator reads it.
type resourceBlock struct {
	typ, name string
	body      *hclsyntax.Body
}

// Adoptable reads what a scan can find for a module (a registry archive) used with a template's inputs, and
// notes on what it skipped.
func Adoptable(archive []byte, inputs []templates.Input) ([]runnerapi.ScanEntry, []string, error) {
	dir, err := os.MkdirTemp("", "ops-adopt-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	if err := extract(archive, dir); err != nil {
		return nil, nil, err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	slices.Sort(files)
	var res []resourceBlock
	var notes []string
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			return nil, nil, err
		}
		f, diags := hclsyntax.ParseConfig(src, filepath.Base(name), hcl.InitialPos)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Base(name), diags)
		}
		for _, b := range f.Body.(*hclsyntax.Body).Blocks {
			if b.Type == "resource" && len(b.Labels) == 2 {
				res = append(res, resourceBlock{b.Labels[0], b.Labels[1], b.Body})
			}
		}
	}
	g := generator{inputs: inputs, refs: map[string]ref{}}
	addrs := map[string]bool{}
	for _, r := range res {
		addrs[r.typ+"."+r.name] = true
	}
	var out []runnerapi.ScanEntry
	for _, r := range res {
		a, note := g.entry(r, addrs)
		if note != "" {
			notes = append(notes, note)
			continue
		}
		out = append(out, a)
	}
	for i := range out { // related: found from the resource that refers to it
		if rf, ok := g.refs[strings.TrimSuffix(out[i].To, "[0]")]; ok {
			out[i].IDFrom = rf.path
		}
	}
	if !slices.ContainsFunc(out, runnerapi.ScanEntry.Primary) && len(out) > 0 {
		notes = append(notes, "every resource refers to another one: none to find first")
	}
	return out, notes, nil
}

type generator struct {
	inputs []templates.Input
	refs   map[string]ref // resource address → the first resource referring to its id
}

type ref struct{ from, path string }

func (g *generator) input(name string) *templates.Input {
	i := slices.IndexFunc(g.inputs, func(in templates.Input) bool { return in.Name == name })
	if i < 0 {
		return nil
	}
	return &g.inputs[i] // read only
}

// entry is r's adopt entry; a note instead if r can't be adopted as a single resource.
func (g *generator) entry(r resourceBlock, addrs map[string]bool) (runnerapi.ScanEntry, string) {
	addr := r.typ + "." + r.name
	if _, ok := r.body.Attributes["for_each"]; ok {
		return runnerapi.ScanEntry{}, addr + ": for_each isn't supported, skipped"
	}
	a := runnerapi.ScanEntry{To: addr, List: r.typ, Require: requireByType[r.typ], Inputs: map[string]string{}}
	if c, ok := r.body.Attributes["count"]; ok {
		cond, ok := g.countCondition(c.Expr)
		if !ok {
			return runnerapi.ScanEntry{}, addr + ": its count isn't var.x == \"value\" ? 1 : 0, skipped"
		}
		a.To, a.ShowIf = addr+"[0]", cond
	}
	ignored := map[string]bool{}
	for _, b := range r.body.Blocks {
		if b.Type == "lifecycle" {
			if ic, ok := b.Body.Attributes["ignore_changes"]; ok {
				list, _ := hcl.ExprList(ic.Expr)
				for _, e := range list {
					if t, diags := hcl.AbsTraversalForExpr(e); !diags.HasErrors() {
						ignored[t.RootName()] = true
					}
				}
			}
		}
	}
	g.body(r.body, "", addr, addrs, ignored, &a)
	if len(a.Inputs) == 0 {
		a.Inputs = nil
	}
	return a, ""
}

// body maps the attributes and blocks of b (at path prefix) into a.
func (g *generator) body(b *hclsyntax.Body, prefix, addr string, addrs, ignored map[string]bool, a *runnerapi.ScanEntry) {
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range b.Attributes {
			if !yield(k) {
				return
			}
		}
	}) {
		if prefix == "" && slices.Contains([]string{"count", "provider", "depends_on"}, name) {
			continue
		}
		expr, path := b.Attributes[name].Expr, prefix+name
		if other, idPath := resourceID(expr, path, addrs); other != "" && other != addr {
			if _, ok := g.refs[other]; !ok {
				g.refs[other] = ref{addr, idPath}
			}
			continue
		}
		v := varOf(expr)
		in := g.input(v)
		if in == nil {
			continue
		}
		if in.Sensitive {
			if prefix == "" && ignored[name] && !slices.Contains(a.Placeholder, v) {
				a.Placeholder = append(a.Placeholder, v)
			}
			continue // a sensitive value never fills the form
		}
		if _, ok := a.Inputs[v]; !ok {
			a.Inputs[v] = path
		}
	}
	for _, blk := range b.Blocks {
		switch {
		case blk.Type == "lifecycle" || blk.Type == "timeouts" || blk.Type == "provisioner" || blk.Type == "connection":
		case blk.Type == "dynamic" && len(blk.Labels) == 1:
			if fe, ok := blk.Body.Attributes["for_each"]; ok {
				if c, ok := fe.Expr.(*hclsyntax.ConditionalExpr); ok {
					if in := g.input(varOf(c.Condition)); in != nil && in.Type == "bool" {
						a.Inputs[in.Name] = prefix + blk.Labels[0]
					}
				}
			}
		default:
			g.body(blk.Body, prefix+blk.Type+"[0].", addr, addrs, ignored, a)
		}
	}
}

// countCondition reads count = var.x == "v" ? 1 : 0 (either side of ==) or var.x ? 1 : 0 as show_if.
func (g *generator) countCondition(e hclsyntax.Expression) (*templates.ShowIf, bool) {
	c, ok := e.(*hclsyntax.ConditionalExpr)
	if !ok || !isNumber(c.TrueResult, 1) || !isNumber(c.FalseResult, 0) {
		return nil, false
	}
	if in := g.input(varOf(c.Condition)); in != nil && in.Type == "bool" {
		return &templates.ShowIf{Input: in.Name, Equals: []string{"true"}}, true
	}
	eq, ok := c.Condition.(*hclsyntax.BinaryOpExpr)
	if !ok || eq.Op != hclsyntax.OpEqual {
		return nil, false
	}
	name, lit := varOf(eq.LHS), literal(eq.RHS)
	if name == "" {
		name, lit = varOf(eq.RHS), literal(eq.LHS)
	}
	if in := g.input(name); in == nil || lit == "" || in.Type != "select" && in.Type != "string" {
		return nil, false
	}
	return &templates.ShowIf{Input: name, Equals: []string{lit}}, true
}

// resourceID matches [other.res.id] (path[0]) or other.res.id (path) for a resource of the module.
func resourceID(e hclsyntax.Expression, path string, addrs map[string]bool) (string, string) {
	if t, ok := e.(*hclsyntax.TupleConsExpr); ok && len(t.Exprs) == 1 {
		if other, _ := resourceID(t.Exprs[0], path, addrs); other != "" {
			return other, path + "[0]"
		}
		return "", ""
	}
	tr, diags := hcl.AbsTraversalForExpr(e)
	if diags.HasErrors() || len(tr) != 3 {
		return "", ""
	}
	name, ok1 := tr[1].(hcl.TraverseAttr)
	attr, ok2 := tr[2].(hcl.TraverseAttr)
	if other := tr.RootName() + "." + name.Name; ok1 && ok2 && attr.Name == "id" && addrs[other] {
		return other, path
	}
	return "", ""
}

// varOf is x for var.x and coalesce(var.x, ...) / try(var.x, ...), else "".
func varOf(e hclsyntax.Expression) string {
	if f, ok := e.(*hclsyntax.FunctionCallExpr); ok && (f.Name == "coalesce" || f.Name == "try") && len(f.Args) > 0 {
		e = f.Args[0]
	}
	t, diags := hcl.AbsTraversalForExpr(e)
	if diags.HasErrors() || len(t) != 2 || t.RootName() != "var" {
		return ""
	}
	a, _ := t[1].(hcl.TraverseAttr)
	return a.Name
}

func literal(e hclsyntax.Expression) string {
	v, diags := e.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || !v.Type().Equals(cty.String) {
		return ""
	}
	return v.AsString()
}

func isNumber(e hclsyntax.Expression, n int64) bool {
	v, diags := e.Value(nil)
	return !diags.HasErrors() && v.IsKnown() && !v.IsNull() && v.Type().Equals(cty.Number) && v.Equals(cty.NumberIntVal(n)).True()
}
