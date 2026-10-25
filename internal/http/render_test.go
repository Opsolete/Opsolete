package httpserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opsolete/opsolete/internal/auth"
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
