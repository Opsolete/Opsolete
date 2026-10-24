// Package authtest is a minimal OIDC provider for tests: discovery, JWKS, an authorize
// endpoint that approves immediately and a token endpoint that checks PKCE.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const ClientID, ClientSecret = "ops-test", "s3cret"

type Issuer struct {
	*httptest.Server
	Claims map[string]any // extra ID token claims, e.g. "email"
	Nonce  string         // if set, replaces the nonce the client asked for (to test nonce checks)
	mu     sync.Mutex
	codes  map[string][2]string // code -> nonce, PKCE challenge
}

func New(t *testing.T) *Issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{Claims: map[string]any{"email": "dev@example.com"}, codes: map[string][2]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": is.URL, "authorization_endpoint": is.URL + "/authorize",
			"token_endpoint": is.URL + "/token", "jwks_uri": is.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != ClientID || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		is.mu.Lock()
		is.codes[code] = [2]string{q.Get("nonce"), q.Get("code_challenge")}
		is.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		back.RawQuery = url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, secret, _ := r.BasicAuth()
		is.mu.Lock()
		c, ok := is.codes[r.PostForm.Get("code")]
		delete(is.codes, r.PostForm.Get("code"))
		is.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || id != ClientID || secret != ClientSecret || base64.RawURLEncoding.EncodeToString(sum[:]) != c[1] {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": is.URL, "sub": "u1", "aud": ClientID, "nonce": c[0],
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		if is.Nonce != "" {
			claims["nonce"] = is.Nonce
		}
		for k, v := range is.Claims {
			claims[k] = v
		}
		payload, _ := json.Marshal(claims)
		jws, _ := signer.Sign(payload)
		idt, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}
