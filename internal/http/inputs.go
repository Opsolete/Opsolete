package httpserver

import (
	"net/http"

	"github.com/opsolete/opsolete/internal/app"
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

// formDefaults fills a form from stored inputs: sensitive ones stay empty, set lists those with a value.
func formDefaults(t templates.Template, stored map[string]any) (vals map[string]string, set map[string]bool) {
	vals, set = map[string]string{}, map[string]bool{}
	for _, in := range t.Inputs {
		v, ok := stored[in.Name]
		switch {
		case in.Sensitive:
			set[in.Name] = ok
		case ok && !app.IsSealed(v): // a sealed value of a no longer sensitive input must be typed again
			vals[in.Name] = templates.FormatValue(v)
		case !ok:
			vals[in.Name] = in.Default
		}
	}
	return vals, set
}
