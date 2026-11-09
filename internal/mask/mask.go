// Package mask is the one rule for hiding known secrets (the values of sensitive variables and
// inputs) in what Terraform prints and returns: the runner's log and the outputs of runs and states.
// Best-effort: it recognises a secret only as written, at least 4 characters (not counting spaces),
// and a multi-line secret line by line; encoded (base64, JSON-escaped, hashed) or split values pass.
package mask

import (
	"slices"
	"strings"
)

// Sensitive replaces an output value that contains a known secret.
const Sensitive = "(sensitive)"

// Masker holds the strings to hide, longest first.
type Masker struct{ values []string }

// New returns the masker for secrets. A multi-line secret (a PEM key) is printed line by line too,
// so each line counts as one. Longest first: a replacer tries its pairs in order, so a secret
// containing another is masked whole.
func New(secrets []string) Masker {
	var vs []string
	for _, v := range secrets {
		vs = append(vs, v)
		if strings.Contains(v, "\n") {
			vs = append(vs, strings.Split(v, "\n")...)
		}
	}
	slices.SortFunc(vs, func(a, b string) int { return len(b) - len(a) })
	var m Masker
	for _, v := range slices.Compact(vs) {
		if len(strings.TrimSpace(v)) >= 4 { // shortcut: shorter values (true, 1, eu) would mask ordinary output; they stay visible
			m.values = append(m.values, v)
		}
	}
	return m
}

// Pairs are old, new pairs for strings.NewReplacer replacing each secret with with.
func (m Masker) Pairs(with string) []string {
	var p []string
	for _, v := range m.values {
		p = append(p, v, with)
	}
	return p
}

// Hides reports whether s contains a secret.
func (m Masker) Hides(s string) bool {
	return slices.ContainsFunc(m.values, func(v string) bool { return strings.Contains(s, v) })
}

// Value returns a decoded JSON value with every string that contains a secret, also inside lists
// and objects, replaced whole by Sensitive: a partly masked string still leaks its context and is
// of no use. Other strings, numbers and booleans are kept; v itself is not changed.
func (m Masker) Value(v any) any {
	switch t := v.(type) {
	case string:
		if m.Hides(t) {
			return Sensitive
		}
	case []any:
		out := make([]any, len(t))
		for i, c := range t {
			out[i] = m.Value(c)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, c := range t {
			out[k] = m.Value(c)
		}
		return out
	}
	return v
}
