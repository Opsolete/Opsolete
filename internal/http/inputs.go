package httpserver

import (
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/opsolete/opsolete/internal/templates"
)

// secretPrefix names the form fields of sensitive inputs, so the audit log redacts them.
const secretPrefix = "secret."

// formValues reads t's inputs from a posted form.
func formValues(f url.Values, t templates.Template) map[string]string {
	raw := map[string]string{}
	for _, in := range t.Inputs {
		if in.RowFields() != nil {
			raw[in.Name] = listValue(f, in)
			continue
		}
		if in.Type == "object" {
			raw[in.Name] = objectValue(f, in)
			continue
		}
		field := in.Name
		if in.Sensitive {
			field = secretPrefix + in.Name
		}
		raw[in.Name] = f.Get(field)
	}
	return raw
}

// inputLabels names t's inputs and the deploy form's own fields, for the errors at the top of the form.
func inputLabels(t templates.Template) map[string]string {
	m := map[string]string{fieldWorkspace: "Workspace name", fieldProject: "Project"}
	for _, in := range t.Inputs {
		m[in.Name] = in.Title()
	}
	return m
}

// listValue reads a list, string_list or map input's rows from a posted form, fields (RowFields) named
// <list>.<row>.<field> (row a number: the order), as a JSON array of objects. Rows left wholly empty are dropped.
func listValue(f url.Values, in templates.Input) string {
	rows := map[int]map[string]string{}
	for k, vs := range f {
		rest, ok := strings.CutPrefix(k, in.Name+".")
		i, field, _ := strings.Cut(rest, ".")
		n, err := strconv.Atoi(i)
		if !ok || err != nil || n < 0 || !slices.ContainsFunc(in.RowFields(), func(f templates.Input) bool { return f.Name == field }) {
			continue
		}
		if rows[n] == nil {
			rows[n] = map[string]string{}
		}
		rows[n][field] = vs[0]
	}
	out := []map[string]string{}
	for _, n := range slices.Sorted(maps.Keys(rows)) {
		if slices.ContainsFunc(slices.Collect(maps.Values(rows[n])), func(v string) bool { return strings.TrimSpace(v) != "" }) {
			out = append(out, rows[n])
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// objectValue reads an object input's fields from a posted form, named <object>.<field>, as a JSON object.
func objectValue(f url.Values, in templates.Input) string {
	obj := map[string]string{}
	for _, field := range in.Fields {
		if vs, ok := f[in.Name+"."+field.Name]; ok {
			obj[field.Name] = vs[0]
		}
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

// listRows are a list, string_list or map input's rows from its form value, each field as the form shows it: the
// posted rows (objects), or the value as stored, a string_list's strings (a value each) or a map (a key and value
// each, by key).
func listRows(v string) []map[string]string {
	var x any
	json.Unmarshal([]byte(v), &x)
	var rows []any
	switch y := x.(type) {
	case []any:
		rows = y
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(y)) {
			rows = append(rows, map[string]any{"key": k, "value": y[k]})
		}
	}
	out := make([]map[string]string, len(rows))
	for i, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			row = map[string]any{"value": r}
		}
		out[i] = fieldTexts(row)
	}
	return out
}

// objectFields are an object input's fields from its form value (a JSON object), as the form shows them.
func objectFields(v string) map[string]string {
	var obj map[string]any
	json.Unmarshal([]byte(v), &obj)
	return fieldTexts(obj)
}

func fieldTexts(obj map[string]any) map[string]string {
	out := map[string]string{}
	for k, x := range obj {
		if x != nil {
			out[k] = templates.FormatValue(x)
		}
	}
	return out
}
