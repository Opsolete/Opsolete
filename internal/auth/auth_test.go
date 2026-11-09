package auth

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/auth/authtest"
)

var (
	savedGroups   []string  // groups the last login recorded
	savedSubject  string    // subject the last login recorded
	emailTaken    bool      // the save callback answers ErrEmailTaken
	lastPrincipal Principal // who the last callback named for the audit log
)

// memStore keeps sessions in memory, by token hash; users are only recorded.
type memStore struct {
	emails   map[uuid.UUID]string
	sessions map[[32]byte]memSession
}

type memSession struct {
	Session
	NewSession
	exp time.Time
}

func (m *memStore) UpsertUser(_ context.Context, subject, email, role string, groups []string) (uuid.UUID, error) {
	savedGroups, savedSubject = groups, subject
	if emailTaken {
		return uuid.Nil, ErrEmailTaken
	}
	id := uuid.NewSHA1(uuid.Nil, []byte(email))
	m.emails[id] = email
	return id, nil
}

func (m *memStore) CreateSession(_ context.Context, n NewSession) (string, error) {
	token := uuid.NewString()
	m.sessions[sha256.Sum256([]byte(token))] = memSession{Session{uuid.New(), n.UserID, m.emails[n.UserID], n.AdminEmail, n.AdminGroups}, n, time.Now().Add(n.TTL)}
	return token, nil
}

func (m *memStore) Session(_ context.Context, token string) (Session, error) {
	s, ok := m.sessions[sha256.Sum256([]byte(token))]
	if !ok || time.Now().After(s.exp) {
		return Session{}, ErrNoSession
	}
	return s.Session, nil
}

func (m *memStore) DeleteSession(_ context.Context, token string) error {
	delete(m.sessions, sha256.Sum256([]byte(token)))
	return nil
}

func newOIDC(t *testing.T, is *authtest.Issuer) (*Auth, *memStore) {
	pub, _ := url.Parse("https://ops.example.com")
	m := &memStore{map[uuid.UUID]string{}, map[[32]byte]memSession{}}
	a, err := New(context.Background(), Config{Issuer: is.URL, ClientID: authtest.ClientID, ClientSecret: authtest.ClientSecret,
		Admins: []string{" Boss@Example.com"}, AdminGroups: []string{"grp-admins"}, PublicURL: pub}, []byte("k"), m)
	if err != nil {
		t.Fatal(err)
	}
	return a, m
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
	lastPrincipal = Principal{Type: "anonymous"}
	req := httptest.NewRequest("GET", cb.RequestURI(), nil)
	req.Header.Set("User-Agent", "test-browser")
	req = req.WithContext(WithPrincipal(req.Context(), &lastPrincipal))
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
	u, err := a.User(req)
	u.Session = uuid.Nil // random per login
	return u, err == nil
}

func TestOIDCLogin(t *testing.T) {
	is := authtest.New(t)
	a, m := newOIDC(t, is)

	is.Claims["groups"] = []string{"grp-a", "grp-b"}
	rec := login(t, a, is, "/workspaces?x=1", false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/workspaces?x=1" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if u, ok := sessionUser(a, rec); !ok || u != (User{ID: uuid.NewSHA1(uuid.Nil, []byte("dev@example.com")), Email: "dev@example.com", Role: "user"}) {
		t.Fatalf("session: %+v %v", u, ok)
	}
	if lastPrincipal.Email != "dev@example.com" || lastPrincipal.Type != "user" {
		t.Fatalf("the sign-in must name its user for the audit log: %+v", lastPrincipal)
	}
	if strings.Join(savedGroups, ",") != "grp-a,grp-b" {
		t.Fatalf("the IdP's groups must be recorded at login (for team access): %v", savedGroups)
	}

	is.Claims["email"] = "boss@example.com"
	if u, _ := sessionUser(a, login(t, a, is, "/", false)); !u.Admin() {
		t.Fatalf("ADMIN_EMAILS must grant admin (case-insensitive): %+v", u)
	}
	delete(is.Claims, "email") // preferred_username may be user-editable: it never makes an admin
	is.Claims["preferred_username"] = "boss@example.com"
	if u, ok := sessionUser(a, login(t, a, is, "/", false)); !ok || u.Email != "boss@example.com" || u.Admin() {
		t.Fatalf("preferred_username must not match ADMIN_EMAILS: %+v %v", u, ok)
	}
	is.Claims["groups"] = []string{"grp-a", "grp-admins"}
	rec = login(t, a, is, "/", false)
	if u, _ := sessionUser(a, rec); !u.Admin() {
		t.Fatalf("ADMIN_GROUPS must grant admin: %+v", u)
	}
	recorded := false
	for _, s := range m.sessions {
		recorded = recorded || slices.Equal(s.Session.AdminGroups, []string{"grp-admins"})
	}
	if !recorded {
		t.Fatal("the session records only the login's ADMIN_GROUPS groups")
	}
	a.adminGroups = nil // removed from ADMIN_GROUPS (a restart): the same session is no longer admin
	if u, _ := sessionUser(a, rec); u.Admin() {
		t.Fatalf("the session's role must follow ADMIN_GROUPS: %+v", u)
	}
	if rec := login(t, a, is, "//evil.example", false); rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect: %s", rec.Header().Get("Location"))
	}
}

func TestOIDCLoginRejects(t *testing.T) {
	is := authtest.New(t)
	a, _ := newOIDC(t, is)
	if rec := login(t, a, is, "/", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged state: %d", rec.Code)
	}
	is.Claims["email_verified"] = false
	if rec := login(t, a, is, "/", false); rec.Code != http.StatusForbidden {
		t.Fatalf("unverified email: %d", rec.Code)
	}
	delete(is.Claims, "email_verified") // absent (Entra ID never sends it) is accepted
	if rec := login(t, a, is, "/", false); rec.Code != http.StatusSeeOther || savedSubject != is.URL+" u1" {
		t.Fatalf("login without email_verified: %d, subject %q", rec.Code, savedSubject)
	}
	emailTaken = true
	rec := login(t, a, is, "/", false)
	emailTaken = false
	if rec.Code != http.StatusForbidden {
		t.Fatalf("email of another subject: %d", rec.Code)
	}
	is.Nonce = "replayed"
	if rec := login(t, a, is, "/", false); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong nonce: %d", rec.Code)
	}
}

// TestSessionCookie (A2): the cookie is only a random token; logout deletes the session, so a copy
// of the cookie stops working too; an expired session is no login.
func TestSessionCookie(t *testing.T) {
	is := authtest.New(t)
	a, m := newOIDC(t, is)
	rec := login(t, a, is, "/", false)
	c := rec.Result().Cookies()
	if len(c) != 2 || c[1].Name != sessionCookie || !c[1].HttpOnly || !c[1].Secure || c[1].SameSite != http.SameSiteLaxMode || c[1].Path != "/" {
		t.Fatalf("session cookie: %+v", c)
	}
	if strings.Contains(c[1].Value, ".") || len(m.sessions) != 1 {
		t.Fatalf("the cookie must be an opaque token of a stored session: %q, %d sessions", c[1].Value, len(m.sessions))
	}
	for _, s := range m.sessions {
		if s.UserAgent != "test-browser" || s.IP != "192.0.2.1" || s.TTL != sessionTTL {
			t.Fatalf("session recorded: %+v", s.NewSession)
		}
	}
	if _, ok := sessionUser(a, rec); !ok {
		t.Fatal("signed in")
	}
	mux := http.NewServeMux()
	a.Routes(mux)
	out := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/auth/logout", nil)
	req.AddCookie(c[1])
	mux.ServeHTTP(out, req)
	if out.Code != http.StatusSeeOther || len(m.sessions) != 0 {
		t.Fatalf("logout must delete the session: %d, %d left", out.Code, len(m.sessions))
	}
	if _, ok := sessionUser(a, rec); ok {
		t.Fatal("a copy of the cookie must stop working after logout")
	}
	for _, v := range []string{"", "forged"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: v})
		if _, err := a.User(req); err != ErrNoSession {
			t.Fatalf("unknown token %q: %v", v, err)
		}
	}
	rec = login(t, a, is, "/", false)
	for h, s := range m.sessions {
		s.exp = time.Now().Add(-time.Second)
		m.sessions[h] = s
	}
	if _, ok := sessionUser(a, rec); ok {
		t.Fatal("expired session accepted")
	}
}

func TestDevUser(t *testing.T) {
	m := &memStore{map[uuid.UUID]string{}, map[[32]byte]memSession{}}
	pub, _ := url.Parse("http://localhost")
	a, err := New(context.Background(), Config{DevUser: "Dev@Example.com", PublicURL: pub}, []byte("k"), m)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.User(httptest.NewRequest("GET", "/", nil))
	if err != nil || u.Email != "dev@example.com" || u.Session != uuid.Nil || len(m.sessions) != 0 {
		t.Fatalf("dev user: %+v %v, %d sessions", u, err, len(m.sessions))
	}
	mux := http.NewServeMux()
	a.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/auth/logout", nil))
	if _, err := a.User(httptest.NewRequest("GET", "/", nil)); rec.Code != http.StatusSeeOther || err != nil {
		t.Fatalf("dev mode logout: %d %v", rec.Code, err)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{"/workspaces?x=1": "/workspaces?x=1", "": "/", "https://evil.com": "/",
		"//evil.com": "/", "/\\evil.com": "/", "/\t/evil.com": "/", "/\n/evil.com": "/"} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}
