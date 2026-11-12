package runner

import (
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// found reads, from a plan that imports, what the imported resources are: per template module (mods), each input
// that alone sets an imported attribute gets the attribute's real value (the import's "before"), so the form can
// take it ("Use found values"). Terraform says which variable an attribute reads (the configuration's references,
// followed up through module calls); an input already set must also be what the plan sets there, since a
// reference can't tell var.x from "${var.x}-nic". A list input's rows (a module call's for_each over it, keyed by
// a row's field) get their fields filled in. Sensitive attributes, sensitive variables and secrets are left out.
func found(p *tfjson.Plan, mods []runnerapi.Module, secrets mask.Masker) map[string]map[string]any {
	if p.Config == nil || p.Config.RootModule == nil {
		return nil
	}
	inputs := map[string]map[string]any{}
	for _, m := range mods {
		inputs[m.Name] = m.Inputs
	}
	out := map[string]map[string]any{}
	rows := map[string]map[string][]any{} // module → list input → its rows, as filled in
	keys := map[string][]string{}         // a repeated call's address → its instances' keys
	for _, rc := range p.ResourceChanges {
		if call, key, ok := instanceOf(rc.ModuleAddress); ok && !slices.Contains(keys[call], key) {
			keys[call] = append(keys[call], key)
		}
	}
	for _, rc := range p.ResourceChanges {
		if rc.Change == nil || rc.Change.Importing == nil || rc.Mode != tfjson.ManagedResourceMode {
			continue
		}
		steps := moduleSteps(rc.ModuleAddress)
		if len(steps) == 0 || inputs[steps[0].name] == nil {
			continue
		}
		mod := steps[0].name
		calls := make([]*tfjson.ModuleCall, len(steps)) // calls[i]: the call of steps[i]
		cm := p.Config.RootModule
		for i, s := range steps {
			if cm == nil || cm.ModuleCalls[s.name] == nil {
				cm = nil
				break
			}
			calls[i], cm = cm.ModuleCalls[s.name], cm.ModuleCalls[s.name].Module
		}
		if cm == nil {
			continue
		}
		i := slices.IndexFunc(cm.Resources, func(r *tfjson.ConfigResource) bool {
			return r.Mode == tfjson.ManagedResourceMode && r.Address == rc.Type+"."+rc.Name
		})
		if i < 0 {
			continue
		}
		before, _ := rc.Change.Before.(map[string]any)
		after, _ := rc.Change.After.(map[string]any)
		var walk func(exprs map[string]*tfjson.Expression, before, after map[string]any, sens any)
		walk = func(exprs map[string]*tfjson.Expression, before, after map[string]any, sens any) {
			for attr, e := range exprs {
				if e == nil || sensitive(sens, attr) {
					continue
				}
				if len(e.NestedBlocks) > 0 { // a block written once: its attributes
					b, _ := before[attr].([]any)
					a, _ := after[attr].([]any)
					if len(e.NestedBlocks) == 1 && len(b) == 1 && len(a) == 1 {
						bm, _ := b[0].(map[string]any)
						am, _ := a[0].(map[string]any)
						walk(e.NestedBlocks[0], bm, am, index(sensOf(sens, attr), 0))
					}
					continue
				}
				real, ok := before[attr]
				if s, isStr := real.(string); !ok || empty(real) || isStr && secrets.Hides(s) {
					continue
				}
				in, field, ok := source(e.References, calls, steps)
				if !ok || sensitiveVar(calls[0], in) {
					continue
				}
				planned, known := after[attr]
				if field == "" {
					if cur := inputs[mod][in]; cur != nil && (!known || !same(planned, cur)) {
						continue // set, and not what the plan sets there: built from it, not it
					}
					if _, done := out[mod][in]; !done {
						if out[mod] == nil {
							out[mod] = map[string]any{}
						}
						out[mod][in] = real
					}
					continue
				}
				// a row field: the row of the instance's key (the module call's for_each over list input in)
				if rows[mod] == nil {
					rows[mod] = map[string][]any{}
				}
				if rows[mod][in] == nil {
					rows[mod][in] = clone(inputs[mod][in])
				}
				call, _, _ := instanceOf(rc.ModuleAddress)
				row := keyRow(rows[mod][in], steps[len(steps)-1].key, keys[call])
				if row == nil {
					continue
				}
				if cur := row[field]; cur != nil && (!known || !same(planned, cur)) {
					continue
				}
				row[field] = real
			}
		}
		walk(cm.Resources[i].Expressions, before, after, rc.Change.BeforeSensitive)
	}
	for mod, lists := range rows {
		for in, rs := range lists {
			if out[mod] == nil {
				out[mod] = map[string]any{}
			}
			out[mod][in] = rs
		}
	}
	return out
}

type step struct{ name, key string }

var stepRe = regexp.MustCompile(`module\.([^.\[]+)(?:\[("?)([^\]"]*)"?\])?`)

// moduleSteps splits module.vm.module.data_disk["0"] into its calls and their instance keys.
func moduleSteps(addr string) []step {
	var out []step
	for _, m := range stepRe.FindAllStringSubmatch(addr, -1) {
		out = append(out, step{m[1], m[3]})
	}
	return out
}

// source follows refs (an attribute's references, in the module of the last call) up to the template's input
// that alone sets it: an input, or a field of a list input's rows (field set; in is then the list). false if
// anything else is involved (several variables, locals, other resources, an object's attribute).
func source(refs []string, calls []*tfjson.ModuleCall, steps []step) (in, field string, ok bool) {
	for k := len(calls) - 1; ; k-- {
		v, f, ok := single(refs)
		switch {
		case !ok:
			return "", "", false
		case f != "": // each.value.<f> in the arguments of calls[k+1], repeated over a list input
			if k+1 >= len(calls) || calls[k+1].ForEachExpression == nil || k != 0 {
				return "", "", false
			}
			return listOf(calls[k+1]), f, listOf(calls[k+1]) != ""
		case k == 0: // a variable of the template's module: the input
			return v, "", true
		}
		e := calls[k].Expressions[v]
		if e == nil {
			return "", "", false
		}
		refs = e.References
	}
}

// single reads references as one variable (var.x) or one row field (each.value.f); false otherwise.
func single(refs []string) (v, field string, ok bool) {
	var vars, fields []string
	for _, r := range refs {
		switch parts := strings.Split(r, "."); {
		case r == "each.value":
		case parts[0] == "var" && len(parts) == 2:
			vars = append(vars, parts[1])
		case parts[0] == "each" && len(parts) == 3 && parts[1] == "value":
			fields = append(fields, parts[2])
		case parts[0] == "var": // var.x.y: an attribute of an object
			return "", "", false
		default:
			return "", "", false
		}
	}
	switch {
	case len(vars) == 1 && len(fields) == 0:
		return vars[0], "", true
	case len(fields) == 1 && len(vars) == 0:
		return "", fields[0], true
	}
	return "", "", false
}

// listOf is the template input a repeated call iterates: its for_each's one variable, also through a local
// (shortcut: a local's expression isn't in the plan JSON, so a local named like the variable, local.x over
// var.x, is taken as that variable; other names aren't followed).
func listOf(c *tfjson.ModuleCall) string {
	for _, r := range c.ForEachExpression.References {
		if parts := strings.Split(r, "."); len(parts) == 2 && (parts[0] == "var" || parts[0] == "local") {
			return parts[1]
		}
	}
	return ""
}

// instanceOf splits module.vm.module.data_disk["0"] into the repeated call's address and the instance key.
func instanceOf(addr string) (call, key string, ok bool) {
	i := strings.LastIndex(addr, "[")
	if i < 0 || !strings.HasSuffix(addr, "]") {
		return "", "", false
	}
	return addr[:i], strings.Trim(addr[i+1:len(addr)-1], `"`), true
}

// keyRow is the row of instance key of a for_each keyed by a row field (tostring(d.lun)); nil if none or
// ambiguous. The field is one whose values give each of the call's keys (all) exactly one row: another field
// may hold the key by chance (a disk of size 1 beside LUN 1).
func keyRow(rows []any, key string, all []string) map[string]any {
	fields := map[string]bool{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		for f := range row {
			fields[f] = true
		}
	}
	at := func(f, k string) int { // the one row whose f is k; -1 none or several
		hit := -1
		for i, r := range rows {
			row, _ := r.(map[string]any)
			if b, err := json.Marshal(row[f]); err == nil && (string(b) == k || string(b) == `"`+k+`"`) {
				if hit >= 0 {
					return -1
				}
				hit = i
			}
		}
		return hit
	}
	pick := -1
	for f := range fields {
		if slices.ContainsFunc(all, func(k string) bool { return at(f, k) < 0 }) {
			continue
		}
		if i := at(f, key); pick >= 0 && i != pick {
			return nil // two fields fit, pointing at different rows
		} else {
			pick = i
		}
	}
	if pick < 0 {
		return nil
	}
	row, _ := rows[pick].(map[string]any)
	return row
}

func sensitiveVar(call *tfjson.ModuleCall, name string) bool {
	if call == nil || call.Module == nil {
		return false
	}
	v := call.Module.Variables[name]
	return v != nil && v.Sensitive
}

// sensitive: Terraform marks attr sensitive in sens (before_sensitive: true, or an object of marks).
func sensitive(sens any, attr string) bool {
	if b, ok := sens.(bool); ok {
		return b
	}
	return sensOf(sens, attr) == true
}

func sensOf(sens any, attr string) any {
	if m, ok := sens.(map[string]any); ok {
		return m[attr]
	}
	return sens
}

func index(sens any, i int) any {
	if l, ok := sens.([]any); ok && i < len(l) {
		return l[i]
	}
	if b, ok := sens.(bool); ok {
		return b
	}
	return nil
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// same compares a planned value with an input's, as JSON (numbers are float64 in both).
func same(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return reflect.DeepEqual(x, y)
}

// clone deep-copies a list input's value (JSON-shaped).
func clone(v any) []any {
	b, _ := json.Marshal(v)
	var out []any
	json.Unmarshal(b, &out)
	return out
}
