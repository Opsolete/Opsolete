package httpserver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/templates"
	"github.com/opsolete/opsolete/internal/vcs"
)

// A sensitive input left empty keeps its stored value; one with a rejected new value keeps the error.
func TestSealInputsKeepsRejectedError(t *testing.T) {
	s := &Server{SecretKey: vcs.Key("test-secret-key")}
	tpl := templates.Template{Inputs: []templates.Input{{Name: "pin", Type: "number", Required: true, Sensitive: true}}}
	old := map[string]any{"pin": map[string]any{"sealed": "c3RvcmVk"}}

	for raw, wantErr := range map[string]bool{"": false, "not-a-number": true} {
		form := map[string]string{"pin": raw}
		vals, errs := tpl.Validate(form)
		if err := s.sealInputs(tpl, form, vals, old, errs, uuid.New(), ""); err != nil {
			t.Fatal(err)
		}
		if _, got := errs["pin"]; got != wantErr {
			t.Errorf("%q: error kept = %v, want %v (errs %v)", raw, got, wantErr, errs)
		}
		if _, kept := vals["pin"]; kept == wantErr {
			t.Errorf("%q: stored value used = %v, want %v", raw, kept, !wantErr)
		}
	}
}
