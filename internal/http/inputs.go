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
		if in.Type == "list" {
			raw[in.Name] = listValue(f, in)
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

// listValue reads a list input's rows from a posted form, fields named <list>.<row>.<field> (row a number: the
// order), as the form's value of a list: a JSON array of objects. Rows left wholly empty are dropped.
func listValue(f url.Values, in templates.Input) string {
	rows := map[int]map[string]string{}
	for k, vs := range f {
		rest, ok := strings.CutPrefix(k, in.Name+".")
		i, field, _ := strings.Cut(rest, ".")
		n, err := strconv.Atoi(i)
		if !ok || err != nil || n < 0 || !slices.ContainsFunc(in.Fields, func(f templates.Input) bool { return f.Name == field }) {
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

// listRows are a list input's rows from its form value (a JSON array of objects), each field as the form shows
// it, for the form's rows.
func listRows(v string) []map[string]string {
	var rows []map[string]any
	json.Unmarshal([]byte(v), &rows)
	out := make([]map[string]string, len(rows))
	for i, r := range rows {
		out[i] = map[string]string{}
		for k, x := range r {
			if x != nil {
				out[i][k] = templates.FormatValue(x)
			}
		}
	}
	return out
}
