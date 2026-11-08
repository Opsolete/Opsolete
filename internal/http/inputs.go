package httpserver

import (
	"net/http"

	"github.com/opsolete/opsolete/internal/templates"
)

// secretPrefix names the form fields of sensitive inputs, so the audit log redacts them.
const secretPrefix = "secret."

// formValues reads t's inputs from a posted form.
func formValues(r *http.Request, t templates.Template) map[string]string {
	raw := map[string]string{}
	for _, in := range t.Inputs {
		field := in.Name
		if in.Sensitive {
			field = secretPrefix + in.Name
		}
		raw[in.Name] = r.PostForm.Get(field)
	}
	return raw
}
