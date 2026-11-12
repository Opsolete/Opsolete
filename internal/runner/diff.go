package runner

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"

	"github.com/opsolete/opsolete/internal/mask"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// planDiff lists the attribute changes of the resources summarize lists, what Terraform's plan output shows:
// changed leaf attributes by path, a sensitive value (Terraform's marks, or a string holding one of secrets)
// hidden, a value known only after apply flagged, an attribute that forces replacement marked.
func planDiff(rcs []*tfjson.ResourceChange, secrets mask.Masker) []runnerapi.ResourceDiff {
	var out []runnerapi.ResourceDiff
	for _, rc := range rcs {
		action, ok := actionOf(rc)
		if !ok {
			continue
		}
		d := runnerapi.ResourceDiff{Address: rc.Address, Action: action, Reason: string(rc.ActionReason), Attrs: []runnerapi.Attr{}}
		if rc.Change.Importing != nil {
			d.ImportID = rc.Change.Importing.ID
		}
		w := diffWalker{secrets: secrets, forces: replacePaths(rc.Change.ReplacePaths)}
		w.walk("", rc.Change.Before, rc.Change.After, rc.Change.BeforeSensitive, rc.Change.AfterSensitive, rc.Change.AfterUnknown)
		d.Attrs, d.Truncated = w.attrs, w.truncated
		out = append(out, d)
	}
	return out
}

type diffWalker struct {
	secrets   mask.Masker
	forces    []string
	attrs     []runnerapi.Attr
	truncated bool
}

func (w *diffWalker) walk(path string, before, after, bsens, asens, unknown any) {
	if len(w.attrs) >= runnerapi.MaxDiffAttrs {
		w.truncated = true
		return
	}
	switch {
	case marked(bsens) || marked(asens):
		if !reflect.DeepEqual(before, after) || marked(unknown) {
			w.add(runnerapi.Attr{Path: path, Sensitive: true, HasBefore: before != nil, HasAfter: after != nil || marked(unknown), Unknown: marked(unknown)})
		}
		return
	case marked(unknown):
		a := runnerapi.Attr{Path: path, Unknown: true, HasBefore: before != nil}
		a.Before = w.text(before, &a)
		w.add(a)
		return
	}
	bm, bIsMap := before.(map[string]any)
	am, aIsMap := after.(map[string]any)
	if (bIsMap || before == nil) && (aIsMap || after == nil) && (bIsMap || aIsMap) {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		if um, ok := unknown.(map[string]any); ok { // Terraform leaves values known only after apply out of after
			for k := range um {
				keys[k] = true
			}
		}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			p := k
			if path != "" {
				p = path + "." + k
			}
			w.walk(p, bm[k], am[k], child(bsens, k), child(asens, k), child(unknown, k))
		}
		return
	}
	bl, bIsList := before.([]any)
	al, aIsList := after.([]any)
	if (bIsList || before == nil) && (aIsList || after == nil) && (bIsList || aIsList) {
		for i := range max(len(bl), len(al)) {
			w.walk(fmt.Sprintf("%s[%d]", path, i), at(bl, i), at(al, i), at2(bsens, i), at2(asens, i), at2(unknown, i))
		}
		return
	}
	if reflect.DeepEqual(before, after) {
		return
	}
	a := runnerapi.Attr{Path: path, HasBefore: before != nil, HasAfter: after != nil}
	a.Before, a.After = w.text(before, &a), w.text(after, &a)
	w.add(a)
}

func (w *diffWalker) add(a runnerapi.Attr) {
	for _, f := range w.forces {
		if a.Path == f || strings.HasPrefix(a.Path, f+".") || strings.HasPrefix(a.Path, f+"[") {
			a.Forces = true
		}
	}
	w.attrs = append(w.attrs, a)
}

// text is v as the diff shows it: JSON, cut at MaxDiffValue; one holding a secret makes a sensitive.
func (w *diffWalker) text(v any, a *runnerapi.Attr) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	s := string(b)
	if w.secrets.Hides(s) {
		a.Sensitive = true
	}
	if a.Sensitive {
		a.Before, a.After = "", ""
		return ""
	}
	if len(s) > runnerapi.MaxDiffValue {
		s = s[:runnerapi.MaxDiffValue] + "…"
	}
	return s
}

// replacePaths are the plan's replace_paths in the diff's path form (a.b[0].c).
func replacePaths(rps []any) []string {
	var out []string
	for _, rp := range rps {
		steps, _ := rp.([]any)
		var b strings.Builder
		for _, s := range steps {
			switch x := s.(type) {
			case string:
				if b.Len() > 0 {
					b.WriteByte('.')
				}
				b.WriteString(x)
			case float64:
				fmt.Fprintf(&b, "[%d]", int(x))
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return out
}

// marked: Terraform's mark (sensitive, unknown) is true for this value as a whole.
func marked(m any) bool { b, ok := m.(bool); return ok && b }

func child(m any, k string) any {
	if mm, ok := m.(map[string]any); ok {
		return mm[k]
	}
	return nil
}

func at2(m any, i int) any {
	if l, ok := m.([]any); ok {
		return at(l, i)
	}
	return nil
}

func at(l []any, i int) any {
	if i < len(l) {
		return l[i]
	}
	return nil
}
