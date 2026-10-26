// Package auth handles browser login: OIDC (authorization code + PKCE) or a fixed dev user,
// HMAC-signed session cookies, and the user/admin role.
package auth

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "ops_session"
	loginCookie   = "ops_login"
	// shortcut: role and identity live in the cookie, so role changes and IdP removals apply at the
	// next login (≤ sessionTTL); add a server-side session table if you need instant revocation.
	sessionTTL = 12 * time.Hour
	loginTTL   = 10 * time.Minute
)

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"` // user | admin
}

func (u User) Admin() bool { return u.Role == "admin" }

// Config selects the login method: OIDC, or DevUser (everyone is that user; local dev only).
type Config struct {
	Issuer, ClientID, ClientSecret string
	DevUser                        string
	DevGroups                      []string // the dev user's groups, as an IdP would send them
	Admins                         []string // emails that get the admin role
	PublicURL                      *url.URL // the callback is PublicURL/auth/callback
}

// SaveUser records a login (with the IdP's groups, for team access) and returns the user's id.
type SaveUser func(ctx context.Context, email, role string, groups []string) (string, error)

type Auth struct {
	key    []byte
	secure bool
	admins []string
	save   SaveUser
	dev    *User
	oauth  oauth2.Config
	verify *oidc.IDTokenVerifier
}

// New sets up login. key signs cookies; it must be secret and shared by all replicas.
func New(ctx context.Context, c Config, key []byte, save SaveUser) (*Auth, error) {
	a := &Auth{key: key, secure: c.PublicURL.Scheme == "https", save: save}
	for _, e := range c.Admins {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.admins = append(a.admins, e)
		}
	}
	switch {
	case c.DevUser != "" && c.Issuer != "":
		return nil, errors.New("set either OIDC_ISSUER or AUTH_DEV_USER, not both")
	case c.DevUser != "":
		u := User{Email: strings.ToLower(c.DevUser), Role: a.role(c.DevUser)}
		id, err := save(ctx, u.Email, u.Role, c.DevGroups)
		if err != nil {
			return nil, err
		}
		u.ID = id
		a.dev = &u
		return a, nil
	case c.Issuer == "" || c.ClientID == "" || c.ClientSecret == "":
		return nil, errors.New("login is required: set OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET (or AUTH_DEV_USER for local dev)")
	}
	p, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	a.verify = p.Verifier(&oidc.Config{ClientID: c.ClientID})
	a.oauth = oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: p.Endpoint(),
		RedirectURL: c.PublicURL.JoinPath("/auth/callback").String(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"}}
	return a, nil
}

func (a *Auth) Dev() bool { return a.dev != nil }

func (a *Auth) role(email string) string {
	if slices.Contains(a.admins, strings.ToLower(email)) {
		return "admin"
	}
	return "user"
}

// Routes: GET /auth/login, GET /auth/callback, POST /auth/logout.
func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		a.setCookie(w, sessionCookie, "", "/", -1)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

// User returns the signed-in user of a request.
func (a *Auth) User(r *http.Request) (User, bool) {
	if a.dev != nil {
		return *a.dev, true
	}
	var s struct {
		User
		Exp int64 `json:"exp"`
	}
	if !a.readCookie(r, sessionCookie, &s) || time.Now().Unix() > s.Exp {
		return User{}, false
	}
	return s.User, true
}

type ctxKey struct{}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the user that the login middleware put on the request.
func FromContext(ctx context.Context) User {
	u, _ := ctx.Value(ctxKey{}).(User)
	return u
}

// Principal is who a request acts as, for the audit log. Type is "user" (browser session), "token"
// (a terraform login token) or "anonymous". Handlers that authenticate a request themselves (the
// sign-in callback, API tokens) fill in the one the audit middleware put on the context.
type Principal struct {
	User
	Type string
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func SetPrincipal(ctx context.Context, u User, typ string) {
	if p, ok := ctx.Value(principalKey{}).(*Principal); ok {
		*p = Principal{u, typ}
	}
}

type loginState struct {
	State, Nonce, Verifier, Next string
	Exp                          int64
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if a.dev != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	st := loginState{State: rand.Text(), Nonce: rand.Text(), Verifier: oauth2.GenerateVerifier(), Next: next,
		Exp: time.Now().Add(loginTTL).Unix()}
	a.writeCookie(w, loginCookie, st, "/auth/", loginTTL)
	http.Redirect(w, r, a.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	if a.dev != nil {
		http.NotFound(w, r)
		return
	}
	var st loginState
	q := r.URL.Query()
	if !a.readCookie(r, loginCookie, &st) || time.Now().Unix() > st.Exp ||
		!hmac.Equal([]byte(q.Get("state")), []byte(st.State)) {
		http.Error(w, "login expired or invalid, please try again", http.StatusBadRequest)
		return
	}
	a.setCookie(w, loginCookie, "", "/auth/", -1)
	if e := q.Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusForbidden)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		http.Error(w, "login failed: code exchange", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verify.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != st.Nonce {
		http.Error(w, "login failed: invalid id token", http.StatusForbidden)
		return
	}
	var c struct {
		Email             string   `json:"email"`
		EmailVerified     *bool    `json:"email_verified"`
		PreferredUsername string   `json:"preferred_username"` // Entra ID often has no email claim
		Groups            []string `json:"groups"`             // team access; must be enabled in the IdP
	}
	if err := idt.Claims(&c); err != nil {
		http.Error(w, "login failed: claims", http.StatusForbidden)
		return
	}
	email := strings.ToLower(strings.TrimSpace(cmp.Or(c.Email, c.PreferredUsername)))
	if !strings.Contains(email, "@") || (c.EmailVerified != nil && !*c.EmailVerified) {
		http.Error(w, "login failed: the identity provider sent no verified email", http.StatusForbidden)
		return
	}
	u := User{Email: email, Role: a.role(email)}
	if u.ID, err = a.save(r.Context(), u.Email, u.Role, c.Groups); err != nil {
		http.Error(w, "login failed: could not save user", http.StatusInternalServerError)
		return
	}
	SetPrincipal(r.Context(), u, "user")
	a.writeCookie(w, sessionCookie, struct {
		User
		Exp int64 `json:"exp"`
	}{u, time.Now().Add(sessionTTL).Unix()}, "/", sessionTTL)
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}

// localPath only allows same-site redirects after login.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}

// Cookies are base64(json) + "." + HMAC(name + json), so one cookie can't stand in for another.
func (a *Auth) sign(name string, payload []byte) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte("opsolete cookie " + name + "\x00"))
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) writeCookie(w http.ResponseWriter, name string, v any, path string, ttl time.Duration) {
	b, _ := json.Marshal(v)
	a.setCookie(w, name, base64.RawURLEncoding.EncodeToString(b)+"."+a.sign(name, b), path, int(ttl.Seconds()))
}

func (a *Auth) readCookie(r *http.Request, name string, v any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	enc, sig, ok := strings.Cut(c.Value, ".")
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if !ok || err != nil || !hmac.Equal([]byte(sig), []byte(a.sign(name, b))) {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	// Lax: the IdP's redirect back to /auth/callback is a top-level GET, which Lax allows.
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}
