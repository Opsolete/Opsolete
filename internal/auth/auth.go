// Package auth handles browser login: OIDC (authorization code + PKCE) or a fixed dev user,
// server-side sessions (the cookie holds only a random token), and the user/admin role.
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
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "ops_session"
	loginCookie   = "ops_login"
	// IdP removals apply at the next login (≤ sessionTTL), or at once when the session is ended
	// (logout, log out everywhere, an admin). The role follows ADMIN_EMAILS and ADMIN_GROUPS.
	sessionTTL = 12 * time.Hour
	loginTTL   = 10 * time.Minute
)

type User struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Role  string    `json:"role"` // user | admin
	// Session is the browser session's id (sessions table); zero for the dev user and API tokens.
	Session uuid.UUID `json:"-"`
}

func (u User) Admin() bool { return u.Role == "admin" }

// Config selects the login method: OIDC, or DevUser (everyone is that user; local dev only).
type Config struct {
	Issuer, ClientID, ClientSecret string
	DevUser                        string
	DevGroups                      []string // the dev user's groups, as an IdP would send them
	Admins                         []string // emails that get the admin role
	AdminGroups                    []string // IdP groups that get the admin role
	PublicURL                      *url.URL // the callback is PublicURL/auth/callback
}

// Store keeps users and browser sessions.
type Store interface {
	// UpsertUser records a login (with the IdP's groups, for team access) and returns the user's id.
	// subject is the OIDC issuer and sub, the stable identity ("" for the dev user); email is an attribute.
	UpsertUser(ctx context.Context, subject, email, role string, groups []string) (uuid.UUID, error)
	// CreateSession stores a session and returns its token, the cookie's only content.
	CreateSession(ctx context.Context, s NewSession) (token string, err error)
	// Session is the unexpired session of token with its user, or ErrNoSession.
	Session(ctx context.Context, token string) (Session, error)
	// DeleteSession ends the session of token; an unknown one deletes nothing.
	DeleteSession(ctx context.Context, token string) error
}

// NewSession is a login's session. AdminEmail and AdminGroups are what can make it admin (the email
// claim, the login's ADMIN_GROUPS groups), so the role follows the config on every request.
type NewSession struct {
	UserID        uuid.UUID
	AdminEmail    string
	AdminGroups   []string
	UserAgent, IP string // for the account page, truncated
	TTL           time.Duration
}

// Session is a stored session and its user.
type Session struct {
	ID, UserID  uuid.UUID
	Email       string
	AdminEmail  string
	AdminGroups []string
}

var (
	// ErrEmailTaken is UpsertUser's answer when the email belongs to a user bound to another subject (a reassigned address).
	ErrEmailTaken = errors.New("this email belongs to another identity provider account")
	// ErrNoSession: the request has no session cookie, or its session is unknown, ended or expired.
	ErrNoSession = errors.New("no session")
)

type Auth struct {
	key         []byte
	secure      bool
	admins      []string
	adminGroups []string
	store       Store
	dev         *User
	oauth       oauth2.Config
	verify      *oidc.IDTokenVerifier
}

// New sets up login. key signs the login-state cookie (PKCE verifier and nonce during the IdP redirect);
// it must be secret and shared by all replicas.
func New(ctx context.Context, c Config, key []byte, store Store) (*Auth, error) {
	a := &Auth{key: key, secure: c.PublicURL.Scheme == "https", store: store}
	for _, e := range c.Admins {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.admins = append(a.admins, e)
		}
	}
	a.adminGroups = slices.DeleteFunc(slices.Clone(c.AdminGroups), func(g string) bool { return g == "" })
	switch {
	case c.DevUser != "" && c.Issuer != "":
		return nil, errors.New("set either OIDC_ISSUER or AUTH_DEV_USER, not both")
	case c.DevUser != "":
		u := User{Email: strings.ToLower(c.DevUser), Role: a.Role(c.DevUser, c.DevGroups)}
		id, err := store.UpsertUser(ctx, "", u.Email, u.Role, c.DevGroups)
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

// Role is the role ADMIN_EMAILS and ADMIN_GROUPS give a login; sessions and API tokens take it from
// here, not from what they recorded. email must be the IdP's email claim, never preferred_username,
// which some IdPs (e.g. Keycloak) let users edit.
func (a *Auth) Role(email string, groups []string) string {
	if slices.Contains(a.admins, strings.ToLower(email)) || slices.ContainsFunc(groups, a.adminGroup) {
		return "admin"
	}
	return "user"
}

func (a *Auth) adminGroup(g string) bool { return slices.Contains(a.adminGroups, g) }

// Routes: GET /auth/login, GET /auth/callback, POST /auth/logout, on an *http.ServeMux or a wrapper of one.
func (a *Auth) Routes(mux interface {
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

// logout deletes the session, so a copy of the cookie kept elsewhere stops working too.
func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && a.dev == nil {
		if err := a.store.DeleteSession(r.Context(), c.Value); err != nil {
			http.Error(w, "logout failed, please try again", http.StatusInternalServerError)
			return
		}
	}
	a.setCookie(w, sessionCookie, "", "/", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// User returns the signed-in user of a request: ErrNoSession if there is none, another error if
// the lookup failed.
func (a *Auth) User(r *http.Request) (User, error) {
	if a.dev != nil {
		return *a.dev, nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return User{}, ErrNoSession
	}
	s, err := a.store.Session(r.Context(), c.Value)
	if err != nil {
		return User{}, err
	}
	// the role from the config now, not from the login: an admin removed from ADMIN_EMAILS/GROUPS loses it at the restart
	return User{ID: s.UserID, Email: s.Email, Role: a.Role(s.AdminEmail, s.AdminGroups), Session: s.ID}, nil
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

// CurrentPrincipal is the principal on ctx; anonymous if there is none.
func CurrentPrincipal(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(*Principal); ok {
		return *p
	}
	return Principal{Type: "anonymous"}
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
	// A missing email_verified passes: Entra ID never sends it. Users are keyed by subject, so a
	// reassigned address can't take over an existing account either way.
	if !strings.Contains(email, "@") || (c.EmailVerified != nil && !*c.EmailVerified) {
		http.Error(w, "login failed: the identity provider sent no verified email", http.StatusForbidden)
		return
	}
	claimed := strings.ToLower(strings.TrimSpace(c.Email))
	u := User{Email: email, Role: a.Role(claimed, c.Groups)}
	if u.ID, err = a.store.UpsertUser(r.Context(), idt.Issuer+" "+idt.Subject, u.Email, u.Role, c.Groups); err != nil {
		if errors.Is(err, ErrEmailTaken) {
			http.Error(w, "login failed: "+err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, "login failed: could not save user", http.StatusInternalServerError)
		return
	}
	token, err := a.store.CreateSession(r.Context(), NewSession{UserID: u.ID, AdminEmail: claimed,
		AdminGroups: slices.DeleteFunc(slices.Clone(c.Groups), func(g string) bool { return !a.adminGroup(g) }),
		UserAgent:   truncate(r.UserAgent(), 200), IP: truncate(clientIP(r), 100), TTL: sessionTTL})
	if err != nil {
		http.Error(w, "login failed: could not save the session", http.StatusInternalServerError)
		return
	}
	SetPrincipal(r.Context(), u, "user")
	a.setCookie(w, sessionCookie, token, "/", int(sessionTTL.Seconds()))
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}

// localPath only allows same-site redirects after login. Browsers drop tabs and newlines and read
// "\\" as "/" in URLs, so "/\t/evil.com" would leave the site.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\t\r\n\\") {
		return "/"
	}
	return p
}

// clientIP is the peer address, with X-Forwarded-For in front as sent: only trustworthy behind a proxy that sets it.
func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return f + " (via " + ip + ")"
	}
	return ip
}

// truncate cuts s to at most n bytes of valid UTF-8.
func truncate(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
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
