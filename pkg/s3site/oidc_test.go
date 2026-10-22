package s3site

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rhnvrm/s3site/internal/oidctest"
)

const testPolicy = `{"version":1,"auth":"oidc","allow":{"emails":["alice@example.com"]}}`

type loginFixture struct {
	app      *httptest.Server
	provider *oidctest.Provider
	auth     *OIDCAuth
	sm       *SiteManager
	client   *http.Client
	host     string
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	p, err := oidctest.New("")
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(p)
	t.Cleanup(issuer.Close)
	p.Issuer = issuer.URL
	sm := NewSiteManager(SiteManagerConfig{})
	app := httptest.NewServer(nil)
	t.Cleanup(app.Close)
	cfg := OIDCConfig{Issuer: issuer.URL, ClientID: "s3site-local", CookieKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), RedirectURLs: []string{app.URL + callbackPath}, AllowLocalHTTP: true}
	auth, err := NewOIDCAuth(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(app.URL)
	host := normalizeHost(u.Host)
	mfs := newMemFS()
	mfs.Add("index.html", []byte("private home"))
	mfs.Add("asset.txt", []byte("private asset"))
	mfs.Add("404.html", []byte("private not found"))
	mfs.Add(accessPolicyPath, []byte(testPolicy))
	s, err := prepareSite(&site{hostname: host, fs: mfs})
	if err != nil {
		t.Fatal(err)
	}
	sm.sites[host] = s
	app.Config.Handler = HandlerWithOIDC(sm, nil, "", auth)
	jar, _ := cookiejar.New(nil)
	return &loginFixture{app, p, auth, sm, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, host}
}
func (f *loginFixture) begin(t *testing.T, path string) string {
	t.Helper()
	res, err := f.client.Get(f.app.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 302 {
		t.Fatalf("expected login redirect, got %d", res.StatusCode)
	}
	return res.Header.Get("Location")
}
func (f *loginFixture) authenticate(t *testing.T, location, user string) *http.Response {
	t.Helper()
	res, err := f.client.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "Local test sign-in") {
		t.Fatalf("missing sign in form: %d %s", res.StatusCode, body)
	}
	res, err = f.client.PostForm(location, url.Values{"user": {user}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 {
		t.Fatalf("authorization response: %d", res.StatusCode)
	}
	res, err = f.client.Get(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func TestOIDCLoginFlow(t *testing.T) {
	for _, user := range []string{"alice", "bob", "unverified"} {
		t.Run(user, func(t *testing.T) {
			f := newLoginFixture(t)
			location := f.begin(t, "/asset.txt?download=1")
			res := f.authenticate(t, location, user)
			defer res.Body.Close()
			if user != "alice" {
				if res.StatusCode != 403 {
					t.Fatalf("denied user got %d", res.StatusCode)
				}
				return
			}
			if res.StatusCode != 303 || res.Header.Get("Location") != "/asset.txt?download=1" {
				t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
			}
			for _, tc := range []struct {
				path, body string
				status     int
			}{{"/", "private home", 200}, {"/asset.txt", "private asset", 200}, {"/missing", "private not found", 404}, {"/.s3site/access.json", "private not found", 404}, {"//.s3site/access.json", "private not found", 404}} {
				res, err := f.client.Get(f.app.URL + tc.path)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				if res.StatusCode != tc.status || string(body) != tc.body || res.Header.Get("Cache-Control") != "private, no-store" {
					t.Fatalf("%s: %d %q headers %v", tc.path, res.StatusCode, body, res.Header)
				}
			}
			// A live allowlist update revokes access without waiting for the session to expire.
			f.sm.mu.Lock()
			snapshot := *f.sm.sites[f.host]
			snapshot.policy = &AccessPolicy{Version: 1, Auth: "oidc"}
			f.sm.sites[f.host] = &snapshot
			f.sm.mu.Unlock()
			res, err := f.client.Get(f.app.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 403 {
				t.Fatalf("revoked user got %d", res.StatusCode)
			}
		})
	}
}
func TestOIDCRejectsInvalidIdentityTokens(t *testing.T) {
	for _, kind := range []string{"issuer", "audience", "nonce", "expired", "signature"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			switch kind {
			case "issuer":
				f.provider.TokenIssuer = "https://wrong.example"
			case "audience":
				f.provider.TokenAudience = "wrong"
			case "nonce":
				f.provider.TokenNonce = "wrong"
			case "expired":
				f.provider.Expired = true
			case "signature":
				f.provider.BadSignature = true
			}
			res := f.authenticate(t, f.begin(t, "/"), "alice")
			defer res.Body.Close()
			if res.StatusCode != 401 {
				t.Fatalf("invalid token accepted: %d", res.StatusCode)
			}
		})
	}
}
func TestOIDCRejectsForgedCallbackAndSession(t *testing.T) {
	f := newLoginFixture(t)
	f.begin(t, "/")
	res, err := f.client.Get(f.app.URL + callbackPath + "?state=forged&code=forged")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatalf("forged state: %d", res.StatusCode)
	}
	u, _ := url.Parse(f.app.URL)
	f.client.Jar.SetCookies(u, []*http.Cookie{{Name: "s3site-session", Value: "forged", Path: "/"}})
	f.begin(t, "/")
	w := httptest.NewRecorder()
	if err := f.auth.setCookie(w, f.host, f.app.URL+callbackPath, "session", session{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Expires: time.Now().Add(-time.Minute).Unix()}, time.Hour); err != nil {
		t.Fatal(err)
	}
	f.client.Jar.SetCookies(u, w.Result().Cookies())
	f.begin(t, "/")
}
func TestOIDCAPIAndUnavailable(t *testing.T) {
	f := newLoginFixture(t)
	req, _ := http.NewRequest("GET", f.app.URL+"/asset.txt", nil)
	req.Header.Set("Accept", "application/json")
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("API: %d", res.StatusCode)
	}
	w := httptest.NewRecorder()
	req = httptest.NewRequest("GET", f.app.URL+"/", nil)
	Handler(f.sm, nil, "").ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("missing auth: %d", w.Code)
	}
	delete(f.auth.redirects, f.host)
	w = httptest.NewRecorder()
	HandlerWithOIDC(f.sm, nil, "", f.auth).ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("missing callback: %d", w.Code)
	}
}
func TestOIDCLogout(t *testing.T) {
	f := newLoginFixture(t)
	res := f.authenticate(t, f.begin(t, "/"), "alice")
	res.Body.Close()
	for _, origin := range []string{"https://evil.example", f.app.URL} {
		req, _ := http.NewRequest("POST", f.app.URL+logoutPath, nil)
		req.Header.Set("Origin", origin)
		res, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		want := 403
		if origin == f.app.URL {
			want = 204
		}
		if res.StatusCode != want {
			t.Fatalf("logout: %d", res.StatusCode)
		}
	}
	f.begin(t, "/")
}

func TestOIDCPathRulesAndRoles(t *testing.T) {
	const rules = `{"version":1,"auth":"oidc","rules":[
 {"pathPrefix":"/public","public":true},
 {"pathPrefix":"/team","allow":{"roles":["editor"]}},
 {"path":"/asset.txt","allow":{"emails":["alice@example.com"]}}
 ]}`
	for _, claim := range []string{"roles", "urn:zitadel:iam:org:project:123:roles", "untrusted"} {
		t.Run(claim, func(t *testing.T) {
			f := newLoginFixture(t)
			mfs := newMemFS()
			mfs.Add(accessPolicyPath, []byte(rules))
			mfs.Add("public/index.html", []byte("public"))
			mfs.Add("team/index.html", []byte("team"))
			mfs.Add("asset.txt", []byte("asset"))
			s, err := prepareSite(&site{hostname: f.host, fs: mfs})
			if err != nil {
				t.Fatal(err)
			}
			f.sm.sites[f.host] = s
			f.provider.RolesClaim = "roles"
			f.provider.Roles = []string{"editor"}
			if claim != "untrusted" {
				f.auth.rolesClaim = claim
			}
			if strings.HasPrefix(claim, "urn:") {
				f.provider.RolesClaim = claim
				f.provider.Roles = map[string]any{"editor": map[string]string{"org-id": "example.test"}}
			}
			res, err := f.client.Get(f.app.URL + "/public/")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("public: %d", res.StatusCode)
			}
			res, err = f.client.Get(f.app.URL + "/unmatched")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 403 {
				t.Fatalf("default deny: %d", res.StatusCode)
			}
			res = f.authenticate(t, f.begin(t, "/team/"), "bob")
			res.Body.Close()
			if claim == "untrusted" {
				if res.StatusCode != 403 {
					t.Fatalf("untrusted role accepted: %d", res.StatusCode)
				}
				return
			}
			if res.StatusCode != 303 {
				t.Fatalf("role login: %d", res.StatusCode)
			}
			for _, path := range []string{"/team/", "/team/index.html", "//team/", "/public/../team/", "/%74eam/"} {
				res, err = f.client.Get(f.app.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
				want := 200
				if path == "/team/index.html" {
					want = 301
				}
				if res.StatusCode != want {
					t.Fatalf("role path %s: %d", path, res.StatusCode)
				}
			}
			res, err = f.client.Get(f.app.URL + "/asset.txt")
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != 403 {
				t.Fatalf("role bypassed user allowlist: %d", res.StatusCode)
			}
		})
	}
}

func TestOIDCConfigAndCookieIsolation(t *testing.T) {
	for _, raw := range []string{"http://example.com", "//example.com", "https://user:pass@example.com", "https://example.com?x=1", "https://example.com#fragment"} {
		if validateOIDCURL(raw, true) == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	f := newLoginFixture(t)
	w := httptest.NewRecorder()
	redirect := "https://example.com" + callbackPath
	if err := f.auth.setCookie(w, "example.com", redirect, "session", session{Subject: "alice"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	req := httptest.NewRequest("GET", redirect, nil)
	req.AddCookie(cookie)
	var out session
	if err := f.auth.readCookie(req, "another.example", redirect, "session", &out); err == nil {
		t.Fatal("session accepted on another hostname")
	}
	if err := f.auth.readCookie(req, "example.com", redirect, "login", &out); err == nil {
		t.Fatal("session accepted as transaction")
	}
}

func TestOIDCRejectsExpiredTransactionAndWrongPKCE(t *testing.T) {
	for _, kind := range []string{"expired", "pkce"} {
		t.Run(kind, func(t *testing.T) {
			f := newLoginFixture(t)
			location := f.begin(t, "/")
			req, _ := http.NewRequest("GET", f.app.URL+"/", nil)
			origin, _ := url.Parse(f.app.URL)
			for _, c := range f.client.Jar.Cookies(origin) {
				req.AddCookie(c)
			}
			var tx loginTransaction
			if err := f.auth.readCookie(req, f.host, f.app.URL+callbackPath, "login", &tx); err != nil {
				t.Fatal(err)
			}
			if kind == "expired" {
				tx.Expires = time.Now().Add(-time.Minute).Unix()
			} else {
				tx.Verifier = "wrong-verifier"
			}
			w := httptest.NewRecorder()
			if err := f.auth.setCookie(w, f.host, f.app.URL+callbackPath, "login", tx, 5*time.Minute); err != nil {
				t.Fatal(err)
			}
			f.client.Jar.SetCookies(origin, w.Result().Cookies())
			res := f.authenticate(t, location, "alice")
			defer res.Body.Close()
			want := 401
			if kind == "expired" {
				want = 400
			}
			if res.StatusCode != want {
				t.Fatalf("%s accepted: %d", kind, res.StatusCode)
			}
		})
	}
}

func TestOIDCDoesNotListProtectedFilenames(t *testing.T) {
	f := newLoginFixture(t)
	files := newMemFS()
	files.Add(accessPolicyPath, []byte(`{"version":1,"auth":"oidc","rules":[{"pathPrefix":"/","public":true}]}`))
	files.Add("secret.txt", []byte("secret"))
	s, err := prepareSite(&site{hostname: f.host, fs: files})
	if err != nil {
		t.Fatal(err)
	}
	f.sm.sites[f.host] = s
	res, err := f.client.Get(f.app.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 404 || strings.Contains(string(body), "secret.txt") {
		t.Fatalf("directory leaked: %d %s", res.StatusCode, body)
	}
}
