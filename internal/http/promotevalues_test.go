package httpserver

import (
	"maps"
	"net/url"
	"testing"
)

// promoteValues reads asked-for values by field, a list's rows and an object's fields as JSON, like a template form.
func TestPromoteValues(t *testing.T) {
	f := url.Values{"target": {"prd"}, "digest": {"d"}, "web.size": {"m"}, "secret.web.pw": {"x"},
		"web.disks.1.size": {"64"}, "web.disks.0.size": {"32"}, "web.disks.0.lun": {"0"}, "web.disks.2.size": {" "},
		"web.dns.server": {"10.0.0.5"}}
	want := map[string]string{"web.size": "m", "web.pw": "x", "web.disks": `[{"lun":"0","size":"32"},{"size":"64"}]`,
		"web.dns": `{"server":"10.0.0.5"}`}
	if got := promoteValues(f); !maps.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}
