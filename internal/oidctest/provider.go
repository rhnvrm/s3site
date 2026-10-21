// Package oidctest provides an intentionally insecure local OIDC provider for
// tests and manual development. It must never be exposed to untrusted networks.
package oidctest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

type User struct {
	Subject, Email string
	Verified       bool
}
type authorization struct {
	Client, Redirect, Challenge, Nonce string
	User                               User
}
type Provider struct {
	Issuer string
	// Token overrides allow negative ID token verification tests.
	TokenIssuer, TokenAudience, TokenNonce string
	RolesClaim                             string
	Roles                                  any
	Expired                                bool
	BadSignature                           bool
	key                                    *ecdsa.PrivateKey
	mu                                     sync.Mutex
	codes                                  map[string]authorization
}

func New(issuer string) (*Provider, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Provider{Issuer: issuer, key: key, codes: make(map[string]authorization)}, nil
}
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		writeJSON(w, map[string]any{"issuer": p.Issuer, "authorization_endpoint": p.Issuer + "/authorize", "token_endpoint": p.Issuer + "/token", "jwks_uri": p.Issuer + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"ES256"}})
	case "/keys":
		writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "local", Algorithm: "ES256", Use: "sig"}}})
	case "/authorize":
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = template.Must(template.New("login").Parse(`<h1>Local test sign-in</h1><p>Development only: no password is checked.</p><form method="post" action="{{.}}"><select name="user"><option value="alice">Alice (allowed)</option><option value="bob">Bob (denied)</option><option value="unverified">Alice (unverified email)</option></select><button>Sign in</button></form>`)).Execute(w, r.URL.RequestURI())
			return
		}
		q := r.URL.Query()
		redirect, _ := url.Parse(q.Get("redirect_uri"))
		if redirect == nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/_s3site/callback" || redirect.RawQuery != "" || redirect.Fragment != "" {
			http.Error(w, "invalid redirect", 400)
			return
		}
		if r.Method != http.MethodPost || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
			http.Error(w, "invalid authorization", 400)
			return
		}
		_ = r.ParseForm()
		user := User{Subject: "alice", Email: "alice@example.com", Verified: true}
		switch r.PostForm.Get("user") {
		case "bob":
			user = User{"bob", "bob@example.com", true}
		case "unverified":
			user.Verified = false
		}
		code := oauth2.GenerateVerifier()
		p.mu.Lock()
		p.codes[code] = authorization{q.Get("client_id"), q.Get("redirect_uri"), q.Get("code_challenge"), q.Get("nonce"), user}
		p.mu.Unlock()
		// The demo accepts only this loopback app; tests use the same client ID.
		if q.Get("client_id") != "s3site-local" {
			http.Error(w, "invalid client", 400)
			return
		}
		location := q.Get("redirect_uri") + "?code=" + code + "&state=" + q.Get("state")
		http.Redirect(w, r, location, http.StatusSeeOther)
	case "/token":
		_ = r.ParseForm()
		p.mu.Lock()
		code, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || r.Method != "POST" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != code.Client || r.Form.Get("redirect_uri") != code.Redirect || base64.RawURLEncoding.EncodeToString(hash[:]) != code.Challenge {
			http.Error(w, "invalid grant", 400)
			return
		}
		signingKey := p.key
		if p.BadSignature {
			var keyErr error
			signingKey, keyErr = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if keyErr != nil {
				panic(keyErr)
			}
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: signingKey, KeyID: "local"}}, nil)
		if err != nil {
			panic(err)
		}
		issuer, aud, nonce := p.Issuer, code.Client, code.Nonce
		if p.TokenIssuer != "" {
			issuer = p.TokenIssuer
		}
		if p.TokenAudience != "" {
			aud = p.TokenAudience
		}
		if p.TokenNonce != "" {
			nonce = p.TokenNonce
		}
		expiry := time.Now().Add(time.Hour)
		if p.Expired {
			expiry = time.Now().Add(-time.Hour)
		}
		claimsMap := map[string]any{"iss": issuer, "aud": aud, "sub": code.User.Subject, "iat": time.Now().Unix(), "exp": expiry.Unix(), "nonce": nonce, "email": code.User.Email, "email_verified": code.User.Verified}
		if p.RolesClaim != "" {
			claimsMap[p.RolesClaim] = p.Roles
		}
		claims, _ := json.Marshal(claimsMap)
		signed, err := signer.Sign(claims)
		if err != nil {
			panic(err)
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			panic(err)
		}
		writeJSON(w, map[string]any{"access_token": "local-test-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
	default:
		http.NotFound(w, r)
	}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Print(err)
	}
}
