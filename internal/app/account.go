package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/storage"
)

// A user's own account: the groups of their last login, and their user tokens (`terraform login`),
// which act as them on the cloud API and the registry. Everyone sees and revokes only their own.

const (
	UserTokenTTL = 30 * 24 * time.Hour // users list and revoke their own tokens under Account settings › Tokens
	loginCodeTTL = 5 * time.Minute
)

var errInvalidGrant = reason{ErrForbidden, "invalid_grant"}

// UserGroups are the groups recorded at actor's last login: they decide group access.
func (a *App) UserGroups(ctx context.Context, actor Actor) ([]string, error) {
	raw, err := a.Store.UserGroups(ctx, actor.UserID)
	var groups []string
	if err == nil {
		err = json.Unmarshal(raw, &groups)
	}
	return groups, err
}

// UserTokens lists actor's own tokens.
func (a *App) UserTokens(ctx context.Context, actor Actor) ([]storage.UserToken, error) {
	return a.Store.UserTokens(ctx, actor.UserID)
}

// RevokeUserToken revokes one of actor's own tokens, at once (every request checks it); another
// user's or an unknown one revokes nothing and succeeds.
func (a *App) RevokeUserToken(ctx context.Context, actor Actor, id uuid.UUID) error {
	if err := a.Store.RevokeUserToken(ctx, actor.UserID, id); err != nil {
		return err
	}
	a.Log.Info("token revoked", "id", id, "user", actor.Email)
	return nil
}

// RevokeUserTokens revokes all of actor's own tokens.
func (a *App) RevokeUserTokens(ctx context.Context, actor Actor) error {
	if err := a.Store.RevokeUserTokens(ctx, actor.UserID); err != nil {
		return err
	}
	a.Log.Info("all tokens revoked", "user", actor.Email)
	return nil
}

// TokenUser is the user of an unexpired user token; ErrNotFound for any other token. Role is the
// one recorded at the user's last login: the surface checks it against the current configuration.
func (a *App) TokenUser(ctx context.Context, token string) (id uuid.UUID, email, role string, groups []string, err error) {
	return a.Store.TokenUser(ctx, token)
}

// loginCode is what a `terraform login` authorization code carries (authorization code + PKCE S256).
type loginCode struct {
	User        uuid.UUID `json:"u"`
	RedirectURI string    `json:"r"`
	Challenge   string    `json:"c"`
	Expires     int64     `json:"e"`
}

// LoginCode is the authorization code that sends actor's confirmed `terraform login` to Terraform's
// redirectURI: short-lived, bound to actor and to the PKCE challenge. The surface checks the request.
func (a *App) LoginCode(actor Actor, redirectURI, challenge string) string {
	payload, _ := json.Marshal(loginCode{User: actor.UserID, RedirectURI: redirectURI, Challenge: challenge, Expires: time.Now().Add(loginCodeTTL).Unix()})
	code := base64.RawURLEncoding.EncodeToString(payload)
	return code + "." + a.codeMAC(code)
}

// codeMAC signs an authorization code. shortcut: codes are stateless, so one can be exchanged
// more than once within loginCodeTTL, but only with its PKCE verifier, which only that Terraform has.
func (a *App) codeMAC(code string) string {
	m := hmac.New(sha256.New, a.Registry.Key)
	m.Write([]byte("opsolete oauth code\x00" + code))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RedeemLoginCode exchanges an authorization code, its PKCE verifier and the redirect URI it was
// issued for, for a new user token of the user who confirmed it. ErrForbidden ("invalid_grant"):
// a forged, expired or mismatched code.
func (a *App) RedeemLoginCode(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	payload, mac, _ := strings.Cut(code, ".")
	var c loginCode
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	sum := sha256.Sum256([]byte(verifier))
	if !hmac.Equal([]byte(mac), []byte(a.codeMAC(payload))) || err != nil || json.Unmarshal(raw, &c) != nil ||
		time.Now().Unix() > c.Expires || redirectURI != c.RedirectURI ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != c.Challenge {
		return "", errInvalidGrant
	}
	token, err := a.Store.CreateUserToken(ctx, c.User, "terraform login", UserTokenTTL)
	if err != nil {
		return "", err
	}
	a.Log.Info("user token created", "via", "terraform login", "user", c.User)
	return token, nil
}

// UserSessions lists actor's own unexpired browser sessions.
func (a *App) UserSessions(ctx context.Context, actor Actor) ([]storage.UserSession, error) {
	return a.Store.UserSessions(ctx, actor.UserID)
}

// EndSessions ends every browser session of user, at once (every request looks its session up):
// actor's own (log out everywhere: their `terraform login` tokens stay, under Account settings ›
// Tokens), or anyone's for an admin, which revokes the user's tokens too: an admin cutting someone
// off (they left, or their laptop was stolen) means the CLI as well.
func (a *App) EndSessions(ctx context.Context, actor Actor, user uuid.UUID) error {
	if user != actor.UserID && !actor.Admin {
		return ErrForbidden
	}
	n, err := a.Store.DeleteUserSessions(ctx, user)
	if err != nil {
		return err
	}
	a.Log.Info("sessions ended", "user", user, "count", n, "by", actor.Email)
	if user == actor.UserID {
		return nil
	}
	if err := a.Store.RevokeUserTokens(ctx, user); err != nil {
		return err
	}
	a.Log.Info("all tokens revoked", "user", user, "by", actor.Email)
	return nil
}
