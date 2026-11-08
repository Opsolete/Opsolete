package app

import (
	"testing"

	"github.com/opsolete/opsolete/internal/vcs"
)

// TestLatestTag: tag-based workspaces run the matching tag with the highest version, not the newest-listed.
func TestLatestTag(t *testing.T) {
	tags := []vcs.Tag{{Name: "1.2.0", Commit: "a"}, {Name: "1.10.0-rc", Commit: "f"}, {Name: "1.10.0", Commit: "b"}, {Name: "v-2.0.0", Commit: "c"}, {Name: "1.9.0-rc", Commit: "d"}, {Name: "release", Commit: "e"}}
	for _, c := range []struct{ pattern, want string }{
		{`^\d+\.\d+\.\d+$`, "1.10.0"}, // semantic versioning: 1.10.0 beats 1.2.0 (not a string compare)
		{`\d+\.\d+\.\d+$`, "v-2.0.0"}, // with a prefix
		{`^\d+\.\d+\.\d+`, "1.10.0"},  // with a suffix: 1.10.0-rc, listed first, is lower
		{`-rc$`, "1.10.0-rc"},
		{`^release$`, ""}, // no version number: never picked
		{`(`, ""},         // bad pattern
	} {
		got, ok := latestTag(tags, c.pattern)
		if got.Name != c.want || ok != (c.want != "") {
			t.Errorf("latestTag(%q) = %q %v, want %q", c.pattern, got.Name, ok, c.want)
		}
	}
}
