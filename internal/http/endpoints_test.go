package httpserver

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/opsolete/opsolete/internal/app"
)

// TestEndpointsDocumented: every route the server registers (all features on) is in the endpoints
// page as `METHOD /pattern`, and the page lists no route that doesn't exist.
func TestEndpointsDocumented(t *testing.T) {
	s := New(&app.App{}, nil, Features{Deployments: true, Registry: true, Editor: true, VCS: true})
	s.Handler()
	doc, err := os.ReadFile("../../docs/content/docs/endpoints.md")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, m := range regexp.MustCompile("`((?:GET|POST|PUT|PATCH|DELETE|LOCK|UNLOCK) /[^`]*)`").FindAllStringSubmatch(string(doc), -1) {
		documented = append(documented, m[1])
	}
	for _, p := range s.patterns {
		if !slices.Contains(documented, p) {
			t.Errorf("route %q is not in docs/content/docs/endpoints.md", p)
		}
	}
	for _, d := range documented {
		if !slices.Contains(s.patterns, d) {
			t.Errorf("endpoints.md lists %q, which isn't a route", d)
		}
	}
}
