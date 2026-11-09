package mask

import (
	"reflect"
	"strings"
	"testing"
)

func TestMasker(t *testing.T) {
	m := New([]string{"hunter22", "eu", "-----BEGIN KEY-----\nAAAABBBB\n-----END KEY-----", "hunter22-long"})
	r := strings.NewReplacer(m.Pairs("***")...)
	for in, want := range map[string]string{
		"pw=hunter22-long, hunter22": "pw=***, ***", // longest first
		"region eu":                  "region eu",   // shorter than 4: kept
		"line AAAABBBB":              "line ***",    // a line of a multi-line secret
		"-----BEGIN KEY-----\nAAAABBBB\n-----END KEY-----": "***", // the whole secret
	} {
		if got := r.Replace(in); got != want {
			t.Errorf("Replace(%q) = %q, want %q", in, got, want)
		}
	}
	in := map[string]any{
		"exact":    "hunter22",
		"contains": "postgres://u:hunter22@db",
		"short":    "eu",
		"num":      1.5,
		"nested":   []any{"ok", map[string]any{"pem": "x AAAABBBB y", "fine": "hello"}},
	}
	want := map[string]any{
		"exact":    Sensitive,
		"contains": Sensitive,
		"short":    "eu",
		"num":      1.5,
		"nested":   []any{"ok", map[string]any{"pem": Sensitive, "fine": "hello"}},
	}
	if got := m.Value(in); !reflect.DeepEqual(got, want) {
		t.Errorf("Value = %v, want %v", got, want)
	}
	if in["exact"] != "hunter22" {
		t.Error("Value changed its argument")
	}
	if New(nil).Hides("anything") {
		t.Error("an empty masker hides")
	}
}
