package httpserver

import (
	"net/url"

	"github.com/opsolete/opsolete/internal/templates"
)

// secretPrefix names the form fields of sensitive inputs, so the audit log redacts them.
const secretPrefix = "secret."

// formValues reads t's inputs from a posted form.
func formValues(f url.Values, t templates.Template) map[string]string {
	raw := map[string]string{}
	for _, in := range t.Inputs {
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
