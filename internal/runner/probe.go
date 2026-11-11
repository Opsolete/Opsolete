package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/templates"
)

// Probes (runnerapi.Probe) fill a candidate's optional and repeated parts (data disks, a schedule, a network
// security group): one throwaway plan tries them all, each try a module t<n> calling the part's module directly with
// its import switched on, for a resource of the inventory (or, without a type, once). A failing import fails only
// its try: the errored plan still has the others. A try whose every import succeeds belongs to the candidate: its
// resources' attributes ("before") fill the rows or inputs, with the switch on, so the import plan takes them over.

// maxTries bounds a scan's probe plan, one Azure read per import of a try.
// shortcut: tries past it are skipped (logged); batch them when scans get that large.
const maxTries = 100

// try is one probe of a candidate, for one resource (name, "" none).
type try struct {
	cand  int
	probe runnerapi.Probe
	name  string
	key   string // repeated: the row key it gets in the plan (its index)
}

// probe fills the candidates' parts from tries of their templates' probes (scan[i] is tpls[i]'s), and their
// values vals (for the fit check) with them.
func (r *Runner) probe(ctx context.Context, tf *tfProc, dir string, tpls []templates.Template, scan []runnerapi.ScanTemplate,
	cands []runnerapi.Candidate, vals []map[string]any, inventory []runnerapi.Found, lists []string, w io.Writer) {
	byID := map[string]int{}
	for i, t := range tpls {
		byID[t.ID] = i
	}
	var tries []try
	mods := map[string]any{}
	skipped := 0
	for ci, c := range cands {
		ti := byID[c.Template]
		known := map[string]bool{}
		for _, id := range append(slices.Collect(func(yield func(string) bool) {
			for _, im := range c.Imports {
				yield(im.ID)
			}
		}), c.Uses...) {
			known[strings.ToLower(id)] = true
		}
		for _, p := range scan[ti].Probes {
			var names []string
			if p.Type == "" {
				names = []string{""}
			}
			for _, f := range inventory {
				if strings.EqualFold(f.Type, p.Type) && strings.EqualFold(f.ResourceGroup, rgOf(c.ID)) && !known[strings.ToLower(f.ID)] {
					names = append(names, f.Name)
				}
			}
			for n, name := range names {
				args, ok := probeArgs(tpls[ti], p, c, vals[ci], name, n)
				if !ok {
					break // what it belongs to wasn't found (e.g. the Linux VM for a Windows one)
				}
				if len(tries) == maxTries {
					skipped++
					continue
				}
				if args["source"] = p.Source; p.Version != "" {
					args["version"] = p.Version
				}
				mods["t"+strconv.Itoa(len(tries))] = args
				tries = append(tries, try{cand: ci, probe: p, name: name, key: strconv.Itoa(n)})
			}
		}
	}
	if len(tries) == 0 {
		return
	}
	if skipped > 0 {
		fmt.Fprintf(w, "\n==> %d tries not made: a scan makes at most %d\n", skipped, maxTries)
	}
	fail := func(err error) {
		msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
		fmt.Fprintf(w, "\n==> trying the optional parts failed, they're left for the form: %s\n", msg)
	}
	pdir := filepath.Join(dir, "probe")
	b, err := json.Marshal(map[string]any{"module": mods})
	if err == nil {
		err = os.Mkdir(pdir, 0o700)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(pdir, "probe.tf.json"), b, 0o600)
	}
	if err == nil {
		err = writeProviders(pdir, lists) // the parts' modules configure no provider
	}
	if err != nil {
		fail(err)
		return
	}
	tf.dir = pdir
	fmt.Fprintf(w, "\n==> terraform init (optional parts: %d tries)\n", len(tries))
	if _, err := tf.run(ctx, w, w, "init", "-input=false"); err != nil {
		fail(err)
		return
	}
	// a try for something that isn't there fails its imports: expected, so the plan's errors stay out of the log
	fmt.Fprintln(w, "\n==> terraform plan (optional parts: what imports belongs to its candidate)")
	tf.run(ctx, io.Discard, io.Discard, "plan", "-input=false", "-lock=false", "-out=probe.tfplan", "-compact-warnings")
	var p tfjson.Plan
	if err := tf.json(ctx, &p, "show", "-json", "-no-color", "probe.tfplan"); err != nil {
		fail(err)
		return
	}
	imported := map[string]map[string]*tfjson.ResourceChange{} // module t<n> → its resources imported, by address there
	for _, rc := range p.ResourceChanges {
		mod, rest, ok := strings.Cut(strings.TrimPrefix(rc.Address, "module."), ".")
		if ok && rc.Change != nil && rc.Change.Importing != nil {
			if imported[mod] == nil {
				imported[mod] = map[string]*tfjson.ResourceChange{}
			}
			imported[mod][rest] = rc
		}
	}
	filled := map[int]bool{}
	raws := make([]map[string]string, len(cands))
	for n, t := range tries {
		got := imported["t"+strconv.Itoa(n)]
		if len(got) != t.probe.Imports || len(got) == 0 {
			continue
		}
		if raws[t.cand] == nil {
			raws[t.cand] = maps.Clone(cands[t.cand].Inputs)
		}
		if fillTry(tpls[byID[cands[t.cand].Template]], t, got, raws[t.cand], &cands[t.cand]) {
			filled[t.cand] = true
			fmt.Fprintf(w, "==> %s: %s %s\n", cands[t.cand].Name, t.probe.Call, cmp.Or(t.name, "found"))
		}
	}
	for ci := range filled {
		c, t := &cands[ci], tpls[byID[cands[ci].Template]]
		raw := raws[ci]
		for _, in := range c.Placeholder { // the fit check's placeholders, never in Inputs
			raw[in], _ = vals[ci][in].(string)
		}
		v, errs := t.Validate(raw)
		if len(errs) > 0 {
			fmt.Fprintf(w, "==> %s: the optional parts found don't fit the form, left for it: %v\n", c.Name, errs)
			c.Found = nil
			continue
		}
		vals[ci] = v
		for _, in := range t.Inputs {
			if s, ok := raw[in.Name]; ok && !in.Sensitive && !slices.Contains(c.Placeholder, in.Name) {
				c.Inputs[in.Name] = s
			}
		}
		if c.Fit == "invalid" { // the parts may have been what it lacked
			c.Fit, c.Message = "", ""
		}
	}
}

// probeArgs are a try's module arguments: inputs from the candidate's values (vals), else their defaults or
// placeholders; row fields the try's (the name, the key n, the switch), else their defaults or placeholders; refs
// the IDs the candidate imports. false: a ref the candidate doesn't import.
func probeArgs(t templates.Template, p runnerapi.Probe, c runnerapi.Candidate, vals map[string]any, name string, n int) (map[string]any, bool) {
	inputs := map[string]templates.Input{}
	for _, in := range t.Inputs {
		inputs[in.Name] = in
	}
	fields := map[string]templates.Input{}
	for _, f := range inputs[p.List].RowFields() {
		fields[f.Name] = f
	}
	args := map[string]any{}
	for arg, a := range p.Args {
		var v any
		switch {
		case a.Switch:
			v = true
		case a.Name && name != "":
			v = name
		case len(a.Refs) > 0:
			i := slices.IndexFunc(c.Imports, func(im runnerapi.Import) bool { return slices.Contains(a.Refs, im.To) })
			if i < 0 {
				return nil, false
			}
			v = c.Imports[i].ID
		case a.Field == p.Key && a.Field != "":
			v = typed(fields[a.Field], strconv.Itoa(n))
		case a.Field != "":
			v = typed(fields[a.Field], fields[a.Field].Default)
		case vals[a.Input] != nil:
			v = vals[a.Input]
		default:
			v = typed(inputs[a.Input], inputs[a.Input].Default)
		}
		args[arg] = escapeValue(v)
	}
	return args, true
}

// typed is the value s (a form value; "" the type's placeholder) as in's type has it for Terraform.
func typed(in templates.Input, s string) any {
	if s != "" {
		in.ShowIf, in.Required = nil, false
		if v, errs := (templates.Template{Inputs: []templates.Input{in}}).Validate(map[string]string{in.Name: s}); len(errs) == 0 && v[in.Name] != nil {
			return v[in.Name]
		}
	}
	switch in.Type {
	case "number":
		return 1
	case "bool":
		return false
	case "select":
		if len(in.Options) > 0 {
			return in.Options[0].Value
		}
	case "list", "string_list":
		return []any{}
	case "map", "object":
		return map[string]any{}
	}
	if s := sample(in.Pattern); s != "" { // e.g. HHmm: the provider checks the format before importing
		return s
	}
	return "probe"
}

// sample is a short string the regular expression pattern matches whole, "" if it can't make one.
func sample(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if pattern == "" || err != nil {
		return ""
	}
	var b strings.Builder
	var gen func(*syntax.Regexp)
	gen = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			b.WriteString(string(re.Rune))
		case syntax.OpCharClass:
			if len(re.Rune) > 0 {
				b.WriteRune(re.Rune[0])
			}
		case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
			b.WriteByte('a')
		case syntax.OpCapture, syntax.OpPlus, syntax.OpAlternate: // the first alternative
			gen(re.Sub[0])
		case syntax.OpConcat:
			for _, s := range re.Sub {
				gen(s)
			}
		case syntax.OpRepeat:
			for range re.Min {
				gen(re.Sub[0])
			}
		}
	}
	gen(re.Simplify())
	if s := b.String(); regexp.MustCompile("^(?:"+pattern+")$").MatchString(s) && s != "" {
		return s
	}
	return ""
}

// fillTry fills a belonging try's resources' attributes into raw (the candidate's form values) and its Found:
// repeated, a row of its list input; else its inputs (those the candidate hasn't, and the name) and lists. false if
// it gave nothing (a part tried before already filled its inputs).
func fillTry(t templates.Template, tr try, got map[string]*tfjson.ResourceChange, raw map[string]string, c *runnerapi.Candidate) bool {
	p := tr.probe
	var list templates.Input
	for _, in := range t.Inputs {
		if in.Name == p.List {
			list = in
		}
	}
	isField := func(k string) bool {
		return slices.ContainsFunc(list.RowFields(), func(f templates.Input) bool { return f.Name == k })
	}
	row := map[string]string{}
	for _, a := range p.Args {
		if a.Switch && p.List != "" {
			row[a.Field] = "true"
		} else if a.Switch {
			if raw[a.Input] == "true" {
				return false // a single part: the first try that belongs fills it
			}
			raw[a.Input] = "true"
		}
	}
	for _, f := range p.Fills {
		rc := got[f.To]
		if rc == nil {
			continue
		}
		obj, _ := rc.Change.Before.(map[string]any)
		for k, pth := range f.Values {
			s, ok := rawValue(path(obj, pth))
			switch {
			case !ok:
			case p.List != "" && isField(k):
				row[k] = s
			case p.List == "" && (raw[k] == "" || k == nameInput(p)):
				raw[k] = s
			}
		}
		for in, bl := range f.Lists {
			blocks, _ := path(obj, bl.Block).([]any)
			rows := []map[string]string{}
			for _, blk := range blocks {
				r := map[string]string{}
				for field, attr := range bl.Fields {
					if s, ok := rawValue(path(blk, attr)); ok {
						r[field] = s
					}
				}
				rows = append(rows, r)
			}
			b, _ := json.Marshal(rows)
			raw[in] = string(b)
		}
	}
	key := ""
	if p.List != "" {
		var rows []map[string]string
		json.Unmarshal([]byte(raw[p.List]), &rows)
		if !slices.ContainsFunc(rows, func(r map[string]string) bool { return r[p.Key] == row[p.Key] }) {
			rows = append(rows, row)
		}
		slices.SortFunc(rows, func(a, b map[string]string) int {
			x, _ := strconv.ParseFloat(a[p.Key], 64)
			y, _ := strconv.ParseFloat(b[p.Key], 64)
			return cmp.Compare(x, y)
		})
		b, _ := json.Marshal(rows)
		raw[p.List] = string(b)
		key = "[" + strconv.Quote(row[p.Key]) + "]" // from the cloud: quoted, as an address has it
	} else {
		key = "[0]"
	}
	for _, f := range p.Fills {
		if rc := got[f.To]; rc != nil {
			c.Found = append(c.Found, runnerapi.Import{To: p.Call + key + "." + f.To, ID: rc.Change.Importing.ID})
		}
	}
	return true
}

// nameInput is the input a single part's tried name goes to (its Name argument's), "" none.
func nameInput(p runnerapi.Probe) string {
	for _, a := range p.Args {
		if a.Name {
			return a.Input
		}
	}
	return ""
}

// rgOf is the resource group in an Azure resource ID; "" if none.
func rgOf(id string) string {
	segs := strings.Split(id, "/")
	for i := 0; i+1 < len(segs); i++ {
		if strings.EqualFold(segs[i], "resourceGroups") {
			return segs[i+1]
		}
	}
	return ""
}
