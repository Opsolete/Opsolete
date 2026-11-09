package httpserver

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/app"
)

var (
	inlineHandler = regexp.MustCompile(`<[^>]*\son[a-z]+\s*=`)
	inlineStyle   = regexp.MustCompile(`<[^>]*\sstyle\s*=|<style\b`)
	scriptTag     = regexp.MustCompile(`<script\b[^>]*>`)
	staticURL     = regexp.MustCompile(`/static/[^"]+`)
)

// strictHTML fails a page the CSP would break: inline handlers, styles or scripts, javascript: URLs.
func strictHTML(t *testing.T, page, body string) {
	t.Helper()
	if m := inlineHandler.FindString(body); m != "" {
		t.Errorf("%s: inline event handler: %s", page, m)
	}
	if m := inlineStyle.FindString(body); m != "" {
		t.Errorf("%s: inline style: %s", page, m)
	}
	for _, m := range scriptTag.FindAllString(body, -1) {
		if !strings.Contains(m, " src=") && !strings.Contains(m, `type="application/json"`) {
			t.Errorf("%s: inline script: %s", page, m)
		}
	}
	if strings.Contains(strings.ToLower(body), "javascript:") {
		t.Errorf("%s: a javascript: URL", page)
	}
}

// Every static file a page links is served, typed and cached for good; another hash is not found.
func TestStatic(t *testing.T) {
	s := New(&app.App{Log: testLog(t)}, devAuth(t, nil, "admin@example.com", true), Features{Deployments: true})
	w := httptest.NewRecorder()
	s.render(w, httptest.NewRequest("GET", "/", nil), 200, "authorize.html", &authorizePage{})
	urls := staticURL.FindAllString(w.Body.String(), -1)
	if len(urls) != len(assetURLs) {
		t.Fatalf("the layout links %v, want every static file %v", urls, assetURLs)
	}
	h := s.Handler()
	types := map[string]string{".css": "text/css", ".js": "javascript"}
	for _, u := range urls {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		ct := w.Header().Get("Content-Type")
		if w.Code != 200 || w.Body.Len() == 0 || !strings.Contains(ct, types[u[strings.LastIndex(u, "."):]]) ||
			w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: %d, %d bytes, %q, %q", u, w.Code, w.Body.Len(), ct, w.Header().Get("Cache-Control"))
		}
	}
	for _, u := range []string{"/static/app.000000000000.css", "/static/app.css", "/static/x"} {
		w := httptest.NewRecorder()
		if h.ServeHTTP(w, httptest.NewRequest("GET", u, nil)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", u, w.Code)
		}
	}
}

// HTML pages carry the strict CSP; the authorize page may also post (redirect) to Terraform's callback.
func TestCSP(t *testing.T) {
	h := newServer(t, "../../catalog", false, testStore(t))
	for _, path := range []string{"/", "/projects", "/workspaces", "/account/profile", "/registry"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %q", path, w.Code, w.Header().Get("Content-Type"))
		}
		if got := w.Header().Get("Content-Security-Policy"); got != csp("'self'") {
			t.Errorf("%s: CSP %q", path, got)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s: headers %v", path, w.Header())
		}
		strictHTML(t, path, w.Body.String())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorization?client_id=terraform-cli&response_type=code&code_challenge_method=S256&code_challenge=x&redirect_uri=http%3A%2F%2Flocalhost%3A10000%2Flogin", nil))
	if got := w.Header().Get("Content-Security-Policy"); w.Code != 200 || got != csp("'self' http://localhost:10000") {
		t.Errorf("authorize: %d, CSP %q", w.Code, got)
	}
}
