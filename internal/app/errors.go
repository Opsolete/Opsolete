package app

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/opsolete/opsolete/internal/storage"
)

// Errors of operations. Every surface (UI, cloud API, …) maps them in one error mapper:
//   - ErrNotFound: no such thing, or the actor may not see it (never reveal existence)
//   - ErrForbidden: the actor sees it but may not do this (read-only, admins only)
//   - IsConflict: the thing's current state refuses it (an active run, a taken name, …)
//   - *ValidationError: the input is wrong; Fields name the form fields / API attributes
//   - anything else: an internal error
var (
	ErrNotFound  = storage.ErrNotFound // the same value: storage's "deleted meanwhile" maps the same way
	ErrForbidden = errors.New("forbidden")

	// ErrNoConfig: a run can't start on a workspace without configuration (no template, no repository).
	ErrNoConfig = errors.New("this workspace has no configuration to plan: link a repository first")

	errReadOnly   = reason{ErrForbidden, "you have read-only access to this workspace"}
	errAdminsOnly = reason{ErrForbidden, "admins only"}
)

// reason is a sentinel with a message for the user: errors.Is(err, kind) holds, Error() is msg.
type reason struct {
	kind error
	msg  string
}

func (r reason) Error() string { return r.msg }
func (r reason) Unwrap() error { return r.kind }

// conflicts are the errors that mean "not now, the current state refuses it".
var conflicts = []error{storage.ErrNameTaken, storage.ErrRunActive, storage.ErrNotConfirmable, storage.ErrNotCancelable,
	storage.ErrHasResources, storage.ErrStateLocked, storage.ErrLocalExecution, storage.ErrExists, storage.ErrProjectNameTaken,
	storage.ErrLeaseLost, ErrNoConfig}

// IsConflict reports whether err is a conflict (HTTP 409); its message says why.
func IsConflict(err error) bool {
	return slices.ContainsFunc(conflicts, func(c error) bool { return errors.Is(err, c) })
}

// ValidationError is user input that can't be used: Fields maps form fields (template inputs by
// name, workspace attributes by the Field* names) to what is wrong; Message is about the whole input.
type ValidationError struct {
	Fields  map[string]string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Message != "" || len(e.Fields) == 0 {
		return e.Message
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(e.Fields)) {
		parts = append(parts, e.Fields[k])
	}
	return strings.Join(parts, "; ")
}

// Field names of workspace attributes in a ValidationError. Template input names can't contain "-",
// so these never collide with an input.
const (
	FieldName    = "workspace-name"
	FieldProject = "workspace-project"
)

func invalid(field, msg string) error {
	return &ValidationError{Fields: map[string]string{field: msg}}
}
