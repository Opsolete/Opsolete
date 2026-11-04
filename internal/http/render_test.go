package httpserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/auth"
	"github.com/opsolete/opsolete/internal/templates"
)

// Pooled template clones are rebound per request: concurrent renders show their own user only.
func TestRenderConcurrentUsers(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, Features{}, nil)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			email := fmt.Sprintf("u%d@example.com", i)
			r := httptest.NewRequest("GET", "/", nil)
			r = r.WithContext(auth.WithUser(context.Background(), auth.User{Email: email}))
			w := httptest.NewRecorder()
			s.render(w, r, 200, "authorize.html", map[string]any{"Email": "x", "Days": 30})
			if body := w.Body.String(); !strings.Contains(body, email) || strings.Count(body, "@example.com") != 1 {
				t.Errorf("render for %s shows another user", email)
			}
		})
	}
	wg.Wait()
}

func BenchmarkRender(b *testing.B) {
	s := New(nil, nil, nil, nil, nil, Features{}, nil)
	for b.Loop() {
		s.render(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), 200, "authorize.html", map[string]any{"Email": "a@b", "Days": 30})
	}
}

// The deploy form renders show_if fields hidden and disabled while their condition fails.
func TestRenderShowIf(t *testing.T) {
	tpl := templates.Template{ID: "vm", Name: "VM", Inputs: []templates.Input{
		{Name: "os", Type: "select", Options: []templates.Option{{Value: "windows"}, {Value: "linux"}}},
		{Name: "admin_password", Type: "string", Sensitive: true, ShowIf: &templates.ShowIf{Input: "os", Equals: []string{"windows"}}},
	}}
	s := New(nil, nil, nil, nil, nil, Features{}, nil)
	// "": no value yet counts as the first option, as the browser shows it
	for osv, hidden := range map[string]bool{"linux": true, "windows": false, "": false} {
		w := httptest.NewRecorder()
		s.render(w, httptest.NewRequest("GET", "/", nil), 200, "form.html", formPage{T: tpl, Values: map[string]string{"os": osv}})
		body := w.Body.String()
		if !strings.Contains(body, `name="secret.admin_password"`) {
			t.Fatalf("%s: fields missing:\n%s", osv, body)
		}
		if got := strings.Count(body, " hidden disabled"); got != map[bool]int{true: 1, false: 0}[hidden] {
			t.Errorf("%s: %d hidden, want hidden=%v", osv, got, hidden)
		}
	}
}
