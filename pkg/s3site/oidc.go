package s3site

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const callbackPath = "/_s3site/callback"
const logoutPath = "/_s3site/logout"

// OIDCConfig is operator-controlled, never loaded from a site archive.
// RedirectURLs must list the exact callback URL for every protected hostname.
type OIDCConfig struct {
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"clientId"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	CookieKey    string   `json:"cookieKey"` // base64-encoded 32 random bytes
	RedirectURLs []string `json:"redirectUrls"`
	// AllowLocalHTTP permits HTTP only for localhost/loopback development URLs.
	AllowLocalHTTP bool `json:"allowLocalHttp,omitempty"`
	// RolesClaim names a top-level ID token claim: a string array or Zitadel role map.
	RolesClaim string   `json:"rolesClaim,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
}

type OIDCAuth struct {
	oauth       oauth2.Config
	verifier    *oidc.IDTokenVerifier
	cipher      cipher.AEAD
	redirects   map[string]string
	rolesClaim  string
	cookieScope string
}

type session struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Roles         []string `json:"roles,omitempty"`
	Expires       int64    `json:"exp"`
}
type loginTransaction struct {
	State      string
	Nonce      string
	Verifier   string
	ReturnPath string
	Expires    int64
}

// NewOIDCAuth discovers the provider and validates fixed callback URLs.
func NewOIDCAuth(ctx context.Context, cfg OIDCConfig) (*OIDCAuth, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("OIDC clientId is required")
	}
	if err := validateOIDCURL(cfg.Issuer, cfg.AllowLocalHTTP); err != nil {
		return nil, fmt.Errorf("OIDC issuer: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(cfg.CookieKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("OIDC cookieKey must be base64-encoded 32 random bytes")
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	a := &OIDCAuth{cipher: aead, redirects: make(map[string]string), rolesClaim: cfg.RolesClaim, cookieScope: cfg.Issuer + "\x00" + cfg.ClientID + "\x00" + cfg.RolesClaim}
	for _, raw := range cfg.RedirectURLs {
		if err := validateOIDCURL(raw, cfg.AllowLocalHTTP); err != nil {
			return nil, fmt.Errorf("OIDC redirect: %w", err)
		}
		u, _ := url.Parse(raw)
		if u.Path != callbackPath || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("OIDC redirect must end with %s without query or fragment", callbackPath)
		}
		host := normalizeHost(u.Host)
		if _, exists := a.redirects[host]; exists {
			return nil, fmt.Errorf("duplicate OIDC redirect hostname: %s", host)
		}
		a.redirects[host] = raw
	}
	if len(a.redirects) == 0 {
		return nil, fmt.Errorf("OIDC redirectUrls is required")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	endpoint := provider.Endpoint()
	// Public PKCE clients work with Zitadel's authentication method 'none'.
	if cfg.ClientSecret == "" {
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	a.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: endpoint, Scopes: append([]string{oidc.ScopeOpenID, "email"}, cfg.Scopes...)}
	return a, nil
}

func validateOIDCURL(raw string, local bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid absolute URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if local && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("HTTPS required (HTTP is allowed only on loopback with allowLocalHttp)")
}

func (a *OIDCAuth) cookieName(kind string, redirect string) string {
	if strings.HasPrefix(redirect, "https://") {
		return "__Host-s3site-" + kind
	}
	return "s3site-" + kind
}
func (a *OIDCAuth) setCookie(w http.ResponseWriter, host, redirect, kind string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nonce := make([]byte, a.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := a.cipher.Seal(nonce, nonce, data, []byte(a.cookieScope+"\x00"+host+"\x00"+redirect+"\x00"+kind))
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	if len(encoded) > 3800 {
		return fmt.Errorf("OIDC cookie too large")
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(kind, redirect), Value: encoded, Path: "/", Secure: strings.HasPrefix(redirect, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
	return nil
}
func (a *OIDCAuth) readCookie(r *http.Request, host, redirect, kind string, out any) error {
	c, err := r.Cookie(a.cookieName(kind, redirect))
	if err != nil {
		return err
	}
	data, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(data) < a.cipher.NonceSize() {
		return fmt.Errorf("invalid cookie")
	}
	size := a.cipher.NonceSize()
	plain, err := a.cipher.Open(nil, data[:size], data[size:], []byte(a.cookieScope+"\x00"+host+"\x00"+redirect+"\x00"+kind))
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, out)
}
func (a *OIDCAuth) clearCookie(w http.ResponseWriter, redirect, kind string) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(kind, redirect), Path: "/", Secure: strings.HasPrefix(redirect, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// authorize handles login routes and returns true only for an allowed visitor.
func (a *OIDCAuth) authorize(w http.ResponseWriter, r *http.Request, host string, snapshot *site) bool {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
	decision := snapshot.policy.decision(snapshot.fs, r.URL.Path)
	if r.URL.Path != callbackPath && r.URL.Path != logoutPath && decision.public() {
		return true
	}
	redirect, ok := a.redirects[host]
	if !ok {
		http.Error(w, "authentication unavailable for this site", http.StatusServiceUnavailable)
		return false
	}
	// Match the configured authority, including the port; never trust forwarded headers.
	u, _ := url.Parse(redirect)
	if !strings.EqualFold(r.Host, u.Host) {
		http.Error(w, "invalid authentication host", http.StatusBadRequest)
		return false
	}
	if r.URL.Path == callbackPath {
		a.callback(w, r, host, redirect, snapshot)
		return false
	}
	if r.URL.Path == logoutPath {
		origin := u.Scheme + "://" + u.Host
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		if r.Header.Get("Origin") != origin {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return false
		}
		a.clearCookie(w, redirect, "session")
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	if decision.denied {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	var s session
	if a.readCookie(r, host, redirect, "session", &s) == nil && s.Expires > time.Now().Unix() && s.Subject != "" {
		if decision.permits(s) {
			return true
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	if r.Method != http.MethodGet || strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	tx := loginTransaction{State: oauth2.GenerateVerifier(), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(), ReturnPath: r.URL.RequestURI(), Expires: time.Now().Add(5 * time.Minute).Unix()}
	if err := a.setCookie(w, host, redirect, "login", tx, 5*time.Minute); err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return false
	}
	cfg := a.oauth
	cfg.RedirectURL = redirect
	http.Redirect(w, r, cfg.AuthCodeURL(tx.State, oidc.Nonce(tx.Nonce), oauth2.S256ChallengeOption(tx.Verifier)), http.StatusFound)
	return false
}
func (a *OIDCAuth) callback(w http.ResponseWriter, r *http.Request, host, redirect string, snapshot *site) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tx loginTransaction
	err := a.readCookie(r, host, redirect, "login", &tx)
	a.clearCookie(w, redirect, "login")
	if err != nil || tx.Expires <= time.Now().Unix() || tx.State == "" || subtle.ConstantTimeCompare([]byte(tx.State), []byte(r.URL.Query().Get("state"))) != 1 || r.URL.Query().Get("code") == "" {
		http.Error(w, "invalid or expired login", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cfg := a.oauth
	cfg.RedirectURL = redirect
	token, err := cfg.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(tx.Verifier))
	if err != nil {
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing identity token", http.StatusUnauthorized)
		return
	}
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil || id.Nonce != tx.Nonce || id.Subject == "" {
		http.Error(w, "invalid identity token", http.StatusUnauthorized)
		return
	}
	var identity struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if id.Claims(&identity) != nil {
		http.Error(w, "invalid identity claims", http.StatusUnauthorized)
		return
	}
	s := session{Email: identity.Email, EmailVerified: identity.EmailVerified} // Roles require explicit operator configuration.
	if a.rolesClaim != "" {
		var claims map[string]json.RawMessage
		if id.Claims(&claims) != nil {
			http.Error(w, "invalid role claims", http.StatusUnauthorized)
			return
		}
		if raw, exists := claims[a.rolesClaim]; exists {
			var roles []string
			if err := json.Unmarshal(raw, &roles); err != nil {
				var zitadel map[string]map[string]string
				if err := json.Unmarshal(raw, &zitadel); err != nil {
					http.Error(w, "invalid role claim", http.StatusUnauthorized)
					return
				}
				for role := range zitadel {
					roles = append(roles, role)
				}
			}
			sort.Strings(roles)
			s.Roles = roles
		}
	}
	s.Subject = id.Subject
	expiry := time.Now().Add(8 * time.Hour)
	if id.Expiry.Before(expiry) {
		expiry = id.Expiry
	}
	s.Expires = expiry.Unix()
	target, parseErr := url.ParseRequestURI(tx.ReturnPath)
	if parseErr != nil || !snapshot.policy.decision(snapshot.fs, target.Path).permits(s) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := a.setCookie(w, host, redirect, "session", s, time.Until(expiry)); err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	// Return only to a relative path on this hostname.
	dest := tx.ReturnPath
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.ContainsAny(dest, "\\\r\n") {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
