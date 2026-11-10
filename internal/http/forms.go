package httpserver

import (
	"cmp"
	"errors"
	"html/template"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/app"
	"github.com/opsolete/opsolete/internal/templates"
)

// Forms: every UI POST handler reads its form with readForm (readMultipart for uploads), and shows a
// refused one again with failed: the page it was posted from, the input kept, the errors on it
// (the "formerror" and "fielderror" partials in layout.html).

// maxFormSize caps an urlencoded body; the largest real field is a 64 KB variable value.
const maxFormSize = 1 << 20

// parseForm parses an urlencoded body into r.PostForm once, capped at maxFormSize. The audit
// middleware (to record the form) and readForm both call it, so whichever runs first parses with the
// same limit and the other sees the same values. Other bodies are left to their handlers.
func parseForm(w http.ResponseWriter, r *http.Request) error {
	if r.PostForm != nil {
		return nil
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormSize)
	}
	return r.ParseForm()
}

// badBody answers a body that can't be read: 413 if it's too large, else 400.
func badBody(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, "form too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "invalid form: "+err.Error(), http.StatusBadRequest)
}

// readForm returns the posted form; a body that can't be read is answered (badBody) and ok is false.
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if err := parseForm(w, r); err != nil {
		badBody(w, err)
		return nil, false
	}
	return r.PostForm, true
}

// readMultipart is readForm for a multipart body (file uploads) of at most max bytes; the caller
// removes r.MultipartForm's files. A body over max is left to the page to say (tooLarge: the user
// picked too big a file); any other unreadable body is answered (400) and ok is false.
func readMultipart(w http.ResponseWriter, r *http.Request, max int64) (f url.Values, tooLarge, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	err := r.ParseMultipartForm(1 << 20)
	if _, tooLarge = errors.AsType[*http.MaxBytesError](err); tooLarge {
		return url.Values{}, true, true
	}
	if err != nil {
		badBody(w, err)
		return nil, false, false
	}
	return r.MultipartForm.Value, false, true
}

// trimmed is the form's keys, trimmed; a missing key (an unchecked box) is "".
func trimmed(f url.Values, keys ...string) map[string]string {
	m := make(map[string]string, len(keys))
	for _, k := range keys {
		m[k] = strings.TrimSpace(f.Get(k))
	}
	return m
}

// formState is a refused form on its re-rendered page: which form (a page may have several), the
// user's input, and what is wrong with it. nil on a page that isn't a re-render.
type formState struct {
	Name    string
	Input   url.Values
	Message string            // about the whole input, at the top
	Fields  map[string]string // input name → what is wrong, next to that input
	Labels  map[string]string // optional: input name → its label, for the list at the top
}

// inputNames maps app's field names to the inputs that hold them, where those differ.
var inputNames = map[string]string{app.FieldName: fieldWorkspace, app.FieldProject: fieldProject}

// refused is how the form name, posted as input, shows err: a ValidationError with 422, a conflict
// (its message at the top) with 409. ok is false for any other error, which uiError answers.
func refused(name string, input url.Values, err error) (f *formState, status int, ok bool) {
	f = &formState{Name: name, Input: input}
	if ve, isVE := errors.AsType[*app.ValidationError](err); isVE {
		f.Message, f.Fields = ve.Message, map[string]string{}
		for k, v := range ve.Fields {
			f.Fields[cmp.Or(inputNames[k], k)] = v
		}
		return f, http.StatusUnprocessableEntity, true
	}
	if app.IsConflict(err) {
		f.Message = err.Error()
		return f, http.StatusConflict, true
	}
	return nil, 0, false
}

// failed answers err of the form name: refused re-renders it with page, anything else is uiError's.
// It reports whether there was an error.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, err error, name string, input url.Values, page func(status int, f *formState)) bool {
	if err == nil {
		return false
	}
	if f, status, ok := refused(name, input, err); ok {
		page(status, f)
	} else {
		s.uiError(w, r, err)
	}
	return true
}

// Template funcs of forms; f is the page's formState, nil if it isn't a re-render.

// formOf is f if it is the named form's, else nil: what a form of a page with several reads.
func formOf(f *formState, name string) *formState {
	if f != nil && f.Name == name {
		return f
	}
	return nil
}

// value is the input's posted value on a re-render of its form, else def (the saved value).
func value(f *formState, key string, def any) any {
	if f == nil {
		return def
	}
	return f.Input.Get(key)
}

// checked is whether a checkbox was posted checked on a re-render of its form, else def.
func checked(f *formState, key string, def bool) bool {
	if f == nil {
		return def
	}
	return f.Input.Get(key) != ""
}

// fieldError is what is wrong with the input key, "" if nothing.
func fieldError(f *formState, key string) string {
	if f == nil {
		return ""
	}
	return f.Fields[key]
}

// aria are an input's aria-describedby (its hints, by id, and its error) and aria-invalid attributes.
func aria(f *formState, key string, hints ...string) template.HTMLAttr {
	hints = slices.DeleteFunc(hints, func(h string) bool { return h == "" }) // a hint the input hasn't
	if fieldError(f, key) != "" {
		hints = append(hints, key+"-error")
	}
	var a string
	if len(hints) > 0 {
		a = ` aria-describedby="` + template.HTMLEscapeString(strings.Join(hints, " ")) + `"`
	}
	if fieldError(f, key) != "" {
		a += ` aria-invalid="true"`
	}
	return template.HTMLAttr(a)
}

// List is f's field errors by input name, for the top of the page.
func (f *formState) List() []fieldErr {
	var out []fieldErr
	for _, k := range slices.Sorted(maps.Keys(f.Fields)) {
		out = append(out, fieldErr{k, f.Labels[k], f.Fields[k]})
	}
	return out
}

type fieldErr struct{ Name, Label, Msg string }

var formFuncs = template.FuncMap{"formOf": formOf, "value": value, "checked": checked, "fieldError": fieldError, "aria": aria,
	"adoptField": templates.AdoptField}
