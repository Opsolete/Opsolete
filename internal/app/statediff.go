package app

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strconv"
)

// StateChange is a resource instance or root output that differs between two state versions.
type StateChange struct {
	Address string
	Action  string // create, update or delete, as in plans
	Attrs   []AttrChange
}

// AttrChange is one changed leaf value, JSON encoded; Old or New is "" where the leaf doesn't exist.
type AttrChange struct{ Path, Old, New string }

type leaf struct {
	val    string
	secret bool
}

// secretPath redacts likely secrets that sensitive_attributes doesn't list (states of Terraform < 0.15,
// providers that don't mark them). Over-redacting only hides a value; the change still shows.
var secretPath = regexp.MustCompile(`(?i)password|secret|token|private|credential|connection_string|kube_config|access_key|certificate|(^|[._])key$`)

// stateLeaves flattens a Terraform state file into address -> attribute path -> value, marking
// sensitive values: they never leave the server.
func stateLeaves(body []byte) (map[string]map[string]leaf, error) {
	out := map[string]map[string]leaf{}
	if body == nil {
		return out, nil
	}
	var st struct {
		Outputs map[string]struct {
			Value     any  `json:"value"`
			Sensitive bool `json:"sensitive"`
		} `json:"outputs"`
		Resources []struct {
			Module, Mode, Type, Name string
			Instances                []struct {
				IndexKey       any               `json:"index_key"`
				Deposed        string            `json:"deposed"`
				Attributes     json.RawMessage   `json:"attributes"`
				AttributesFlat map[string]string `json:"attributes_flat"`
				Sensitive      json.RawMessage   `json:"sensitive_attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber() // keep numbers as written: no float rounding of big ids
	if err := d.Decode(&st); err != nil {
		return nil, err
	}
	for name, o := range st.Outputs {
		m := map[string]leaf{}
		walkJSON("value", o.Value, func(p, v string) { m[p] = leaf{v, o.Sensitive || secretPath.MatchString(name)} })
		out["output."+name] = m
	}
	for _, r := range st.Resources {
		for _, in := range r.Instances {
			addr := r.Type + "." + r.Name
			if r.Mode == "data" {
				addr = "data." + addr
			}
			if r.Module != "" {
				addr = r.Module + "." + addr
			}
			switch k := in.IndexKey.(type) {
			case json.Number:
				addr += "[" + k.String() + "]"
			case string:
				addr += "[" + strconv.Quote(k) + "]"
			}
			if in.Deposed != "" {
				addr += " (deposed " + in.Deposed + ")"
			}
			// sensitive_attributes: [[{"type":"get_attr","value":"password"}, ...], ...]; a whole
			// top-level attribute is redacted, and all of them if the paths can't be read.
			var paths [][]struct {
				Type  string
				Value json.RawMessage
			}
			all := len(in.Sensitive) > 0 && json.Unmarshal(in.Sensitive, &paths) != nil
			secret := map[string]bool{}
			for _, p := range paths {
				var name string
				if len(p) == 0 || p[0].Type != "get_attr" || json.Unmarshal(p[0].Value, &name) != nil {
					all = true
					continue
				}
				secret[name] = true
			}
			attrs := map[string]any{}
			for k, v := range in.AttributesFlat { // states of Terraform < 0.12
				attrs[k] = v
			}
			if len(in.Attributes) > 0 {
				d := json.NewDecoder(bytes.NewReader(in.Attributes))
				d.UseNumber()
				if err := d.Decode(&attrs); err != nil {
					return nil, err
				}
			}
			m := map[string]leaf{}
			for name, v := range attrs {
				hide := all || secret[name]
				walkJSON(name, v, func(p, v string) { m[p] = leaf{v, hide || secretPath.MatchString(p)} })
			}
			out[addr] = m
		}
	}
	return out, nil
}

// walkJSON calls emit with the path and JSON encoding of each leaf of v; empty objects and lists are leaves.
func walkJSON(path string, v any, emit func(path, val string)) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			emit(path, "{}")
		}
		for k, c := range t {
			walkJSON(path+"."+k, c, emit)
		}
	case []any:
		if len(t) == 0 {
			emit(path, "[]")
		}
		for i, c := range t {
			walkJSON(path+"."+strconv.Itoa(i), c, emit)
		}
	default:
		b, _ := json.Marshal(t)
		emit(path, string(b))
	}
}

// diffStates lists what changes from state file a to b (nil: no state), sorted by address and path.
func diffStates(a, b []byte) ([]StateChange, error) {
	from, err := stateLeaves(a)
	if err != nil {
		return nil, err
	}
	to, err := stateLeaves(b)
	if err != nil {
		return nil, err
	}
	show := func(l leaf, ok, secret bool) string {
		switch {
		case !ok:
			return ""
		case secret:
			return "(sensitive)"
		}
		return l.val
	}
	var out []StateChange
	for _, addr := range slices.Sorted(maps.Keys(mergeKeys(from, to))) {
		f, inA := from[addr]
		t, inB := to[addr]
		c := StateChange{Address: addr, Action: "update"}
		if !inA {
			c.Action = "create"
		} else if !inB {
			c.Action = "delete"
		}
		for _, p := range slices.Sorted(maps.Keys(mergeKeys(f, t))) {
			x, okA := f[p]
			y, okB := t[p]
			if okA != okB || x.val != y.val {
				secret := x.secret || y.secret // sensitive on either side: an attribute that became sensitive hides its old value too
				c.Attrs = append(c.Attrs, AttrChange{Path: p, Old: show(x, okA, secret), New: show(y, okB, secret)})
			}
		}
		if c.Action != "update" || len(c.Attrs) > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}

func mergeKeys[V any](a, b map[string]V) map[string]bool {
	m := map[string]bool{}
	for k := range a {
		m[k] = true
	}
	for k := range b {
		m[k] = true
	}
	return m
}
