package app

import (
	"maps"

	"github.com/opsolete/opsolete/internal/templates"
)

// A template form as every surface shows it (the deploy, add and configure forms, the builder's preview, a
// promotion's asked-for values): one Field per input, worked out here so the renderers only draw it.

// Field is one input of a template form as the form shows it.
type Field struct {
	templates.Input
	ID      string // the element's id and posted name: the input's name, or <module>.<input> (a promotion)
	Label   string // the input's title, or what the surface calls it ("web: Size")
	Value   string // as the form shows it: posted, stored, the default, or the project's (canonical); "" sensitive
	Visible bool   // show_if holds with the form's values; a hidden field is rendered hidden and disabled
	Locked  string // the project's value as the form says it, when the project sets it (read-only, not posted)
	Set     bool   // sensitive: a value is stored; left empty, it's kept
	Found   string // what an import plan found for it, as the form shows it; "" none
	Hint    string // what the surface says under it instead of the sensitive default; "" none
}

// Form is a template's form: its fields and what they share.
type Form struct {
	T         templates.Template // as the form checks it: ForImport while importing (only the key input required)
	Fields    []Field
	Values    map[string]string // every input's form value by name, the project's in place: show_if's sources
	LockedBy  string            // the project whose values Locked fields show
	Importing bool              // the module imports what exists (Importing)
}

// FormState is what a form shows besides the template: values as typed or loaded (FormDefaults), the
// project's values for its locked inputs (EnvironmentMap.Locked, canonical) and that project's name, the
// sensitive inputs with a stored value, and what an import plan found (FoundValues).
type FormState struct {
	Values  map[string]string
	Locked  map[string]string
	Project string
	Set     map[string]bool
	Found   map[string]any
}

// NewForm works out t's form in state s.
func NewForm(t templates.Template, s FormState) Form {
	f := Form{T: t, Values: maps.Clone(s.Values), LockedBy: s.Project}
	if f.Values == nil {
		f.Values = map[string]string{}
	}
	maps.Copy(f.Values, s.Locked)
	if f.Importing = Importing(t, f.Values); f.Importing {
		f.T = ForImport(t)
	}
	for _, in := range f.T.Inputs {
		fd := Field{Input: in, ID: in.Name, Label: in.Title(), Value: f.Values[in.Name], Visible: f.T.Match(in.ShowIf, f.Values), Set: s.Set[in.Name]}
		if v, ok := s.Locked[in.Name]; ok {
			fd.Locked = ValueText(in, v)
		}
		if v, ok := s.Found[in.Name]; ok {
			fd.Found = templates.FormatValue(v)
		}
		if in.Sensitive {
			fd.Value = ""
		}
		f.Fields = append(f.Fields, fd)
	}
	return f
}

// Field is the form's field of input name; ok false: none.
func (f Form) Field(name string) (Field, bool) {
	for _, fd := range f.Fields {
		if fd.Name == name {
			return fd, true
		}
	}
	return Field{}, false
}
