package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rudesome/form2stack/internal/auth/authtest"
)

var savedGroups []string // groups the last login recorded

func newOIDC(t *testing.T, is *authtest.Issuer) *Auth {
	pub, _ := url.Parse("https://f2s.example.com")
	a, err := New(context.Background(), Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		Admins: []string{" Boss@Example.com"}, PublicURL: pub}, []byte("k"),
		func(_ context.Context, email, role string, groups []string) (string, error) {
			savedGroups = groups
			return "id-" + email, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// login runs the browser side of the flow and returns the callback response.
func login(t *testing.T, a *Auth, is *authtest.Issuer, next string, tamperState bool) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/login?next="+url.QueryEscape(next), nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), is.URL+"/authorize?") {
		t.Fatalf("login: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb, _ := url.Parse(resp.Header.Get("Location"))
	if tamperState {
		q := cb.Query()
		q.Set("state", "forged")
		cb.RawQuery = q.Encode()
	}
	req := httptest.NewRequest("GET", cb.RequestURI(), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func sessionUser(a *Auth, rec *httptest.ResponseRecorder) (User, bool) {
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return a.User(req)
}

func TestOIDCLogin(t *testing.T) {
	is := authtest.New(t)
	a := newOIDC(t, is)

	is.Claims["groups"] = []string{"grp-a", "grp-b"}
	rec := login(t, a, is, "/workspaces?x=1", false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces?x=1" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if u, ok := sessionUser(a, rec); !ok || u != (User{ID: "id-dev@example.com", Email: "dev@example.com", Role: "user"}) {
		t.Fatalf("session: %+v %v", u, ok)
	}
	if strings.Join(savedGroups, ",") != "grp-a,grp-b" {
		t.Fatalf("the IdP's groups must be recorded at login (for team access): %v", savedGroups)
	}

	is.Claims["email"] = "boss@example.com"
	if u, _ := sessionUser(a, login(t, a, is, "/", false)); !u.Admin() {
		t.Fatalf("ADMIN_EMAILS must grant admin (case-insensitive): %+v", u)
	}
	if rec := login(t, a, is, "//evil.example", false); rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %s", rec.Header().Get("Location"))
	}
}

func TestOIDCLoginRejects(t *testing.T) {
	is := authtest.New(t)
	a := newOIDC(t, is)
	if rec := login(t, a, is, "/", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state: %d", rec.Code)
	}
	is.Claims["email_verified"] = false
	if rec := login(t, a, is, "/", false); rec.Code != http.StatusForbidden {
		t.Fatalf("unverified email: %d", rec.Code)
	}
	delete(is.Claims, "email_verified")
	is.Nonce = "replayed"
	if rec := login(t, a, is, "/", false); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong nonce: %d", rec.Code)
	}
}

func TestSessionCookie(t *testing.T) {
	a := &Auth{key: []byte("k")}
	rec := httptest.NewRecorder()
	a.writeCookie(rec, sessionCookie, map[string]any{"email": "a@b.c", "role": "user", "exp": 1 << 40}, "/", sessionTTL)
	c := rec.Result().Cookies()[0]
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	if u, ok := a.User(req); !ok || u.Email != "a@b.c" {
		t.Fatalf("valid cookie rejected: %+v", u)
	}
	// Flip the role in the payload, keep the signature: must be rejected.
	enc, sig, _ := strings.Cut(c.Value, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(enc)
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), `"user"`, `"admin"`, 1)))
	if forged == enc {
		t.Fatal("test is void: payload unchanged")
	}
	for _, v := range []string{forged + "." + sig, enc + "." + sig + "x", enc} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
		if _, ok := a.User(req); ok {
			t.Fatalf("tampered cookie accepted: %s", v)
		}
	}
	expired := httptest.NewRecorder()
	a.writeCookie(expired, sessionCookie, map[string]any{"email": "a@b.c", "exp": 1}, "/", sessionTTL)
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(expired.Result().Cookies()[0])
	if _, ok := a.User(req); ok {
		t.Fatal("expired session accepted")
	}
}
