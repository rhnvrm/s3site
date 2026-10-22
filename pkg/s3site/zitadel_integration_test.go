package s3site

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

// This test runs against real Zitadel, PostgreSQL and S3 containers. Its login
// driver uses the same Session and OIDC APIs as Zitadel Login V2: real passwords,
// sessions, authorization codes, token endpoint and signing keys; no mock tokens.
func TestZitadelIntegration(t *testing.T) {
	issuer := os.Getenv("S3SITE_ZITADEL_ISSUER")
	if testing.Short() || issuer == "" {
		t.Skip("run ./examples/oidc/test-zitadel.sh for real-provider integration")
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatal("Zitadel integration requires an explicit loopback HTTP issuer")
	}
	readToken := func(env string) string {
		t.Helper()
		data, err := os.ReadFile(os.Getenv(env))
		if err != nil {
			t.Fatal(err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			t.Fatalf("empty %s", env)
		}
		return token
	}
	admin := zitadelAPI{issuer, readToken("S3SITE_ZITADEL_ADMIN_PAT_FILE"), &http.Client{Timeout: 20 * time.Second}}
	login := zitadelAPI{issuer, readToken("S3SITE_ZITADEL_LOGIN_PAT_FILE"), &http.Client{Timeout: 20 * time.Second}}
	suffix := fmt.Sprint(time.Now().UnixNano())
	var project struct {
		ProjectID string `json:"id"`
	}
	admin.must(t, "POST", "/management/v1/projects", map[string]any{"name": "s3site-" + suffix, "projectRoleAssertion": true}, &project)
	if project.ProjectID == "" {
		t.Fatal("missing project ID")
	}
	t.Cleanup(func() {
		if err := admin.call("DELETE", "/management/v1/projects/"+project.ProjectID, nil, nil); err != nil {
			t.Error(err)
		}
	})
	admin.must(t, "POST", "/management/v1/projects/"+project.ProjectID+"/roles", map[string]any{"roleKey": "docs-reader", "displayName": "Docs reader"}, nil)
	type testUser struct{ ID, Email string }
	users := map[string]testUser{}
	const password = "LocalIntegrationPassword1!"
	for _, name := range []string{"alice", "bob"} {
		email := name + "-" + suffix + "@example.test"
		var created struct {
			UserID string `json:"userId"`
		}
		admin.must(t, "POST", "/v2/users/human", map[string]any{"username": email, "profile": map[string]string{"givenName": name, "familyName": "Integration"}, "email": map[string]any{"email": email, "isVerified": true}, "password": map[string]any{"password": password, "changeRequired": false}}, &created)
		if created.UserID == "" {
			t.Fatal("missing user ID")
		}
		id := created.UserID
		t.Cleanup(func() {
			if err := admin.call("DELETE", "/v2/users/"+id, nil, nil); err != nil {
				t.Error(err)
			}
		})
		users[name] = testUser{id, email}
	}
	admin.must(t, "POST", "/management/v1/users/"+users["bob"].ID+"/grants", map[string]any{"projectId": project.ProjectID, "roleKeys": []string{"docs-reader"}}, nil)
	// A wrong password must fail at the real identity provider.
	if err := login.call("POST", "/v2/sessions", map[string]any{"checks": map[string]any{"user": map[string]string{"userId": users["alice"].ID}, "password": map[string]string{"password": "WrongPassword1!"}}}, nil); err == nil {
		t.Fatal("Zitadel accepted an invalid password")
	}

	for _, mode := range []StorageMode{StorageMemory, StorageDisk} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			app := httptest.NewServer(nil)
			defer app.Close()
			var registration struct {
				ClientID string `json:"clientId"`
				AppID    string `json:"appId"`
			}
			admin.must(t, "POST", "/management/v1/projects/"+project.ProjectID+"/apps/oidc", map[string]any{
				"name": fmt.Sprintf("s3site-%d", mode), "redirectUris": []string{app.URL + callbackPath}, "responseTypes": []string{"OIDC_RESPONSE_TYPE_CODE"}, "grantTypes": []string{"OIDC_GRANT_TYPE_AUTHORIZATION_CODE"}, "appType": "OIDC_APP_TYPE_WEB", "authMethodType": "OIDC_AUTH_METHOD_TYPE_NONE", "version": "OIDC_VERSION_1_0", "devMode": true, "idTokenRoleAssertion": true, "idTokenUserinfoAssertion": true,
			}, &registration)
			auth, err := NewOIDCAuth(context.Background(), OIDCConfig{Issuer: issuer, ClientID: registration.ClientID, CookieKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), RedirectURLs: []string{app.URL + callbackPath}, AllowLocalHTTP: true, RolesClaim: "urn:zitadel:iam:org:project:" + project.ProjectID + ":roles", Scopes: []string{"urn:zitadel:iam:org:project:id:" + project.ProjectID + ":aud", "urn:zitadel:iam:org:projects:roles"}})
			if err != nil {
				t.Fatal(err)
			}
			s3, bucket := setupS3(t)
			key := fmt.Sprintf("zitadel-%s-%d/site.tar.gz", suffix, mode)
			defer func() {
				if err := s3.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: key}); err != nil {
					t.Error(err)
				}
			}()
			hostURL, _ := url.Parse(app.URL)
			host := normalizeHost(hostURL.Host)
			sm := NewSiteManager(SiteManagerConfig{S3: s3, Bucket: bucket, Storage: mode, DataDir: t.TempDir(), HostedSites: []HostedSite{{Hostname: host, Key: key}}})
			defer func() {
				if s := sm.getSiteSnapshot(host); s != nil {
					s.cleanup()
				}
			}()
			deploy := func(policy string) {
				t.Helper()
				uploadArchive(t, s3, bucket, key, makeTarGz(t, map[string]string{accessPolicyPath: policy, "index.html": "private Zitadel home", "docs/index.html": "role-protected docs"}))
				if err := sm.RefreshHosts([]string{host}); err != nil {
					t.Fatal(err)
				}
			}
			policy, _ := json.Marshal(map[string]any{"version": 1, "auth": "oidc", "rules": []any{
				map[string]any{"pathPrefix": "/docs", "allow": map[string]any{"roles": []string{"docs-reader"}}},
				map[string]any{"pathPrefix": "/", "allow": map[string]any{"emails": []string{users["alice"].Email}}},
			}})
			deploy(string(policy))
			app.Config.Handler = HandlerWithOIDC(sm, nil, "", auth)
			for _, tc := range []struct {
				name, path string
				want       int
			}{{"alice", "/", 303}, {"bob", "/", 403}, {"bob", "/docs/", 303}, {"alice", "/docs/", 403}} {
				t.Run(tc.name+tc.path, func(t *testing.T) {
					jar, _ := cookiejar.New(nil)
					browser := &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
					res := zitadelGet(t, browser, app.URL+tc.path)
					res.Body.Close()
					if res.StatusCode != 302 {
						t.Fatalf("s3site login redirect: %d", res.StatusCode)
					}
					res = zitadelGet(t, browser, res.Header.Get("Location"))
					res.Body.Close()
					if res.StatusCode != 302 {
						t.Fatalf("Zitadel authorization: %d", res.StatusCode)
					}
					loginURL, err := url.Parse(res.Header.Get("Location"))
					if err != nil {
						t.Fatal(err)
					}
					if os.Getenv("S3SITE_ZITADEL_LOGIN_UI") == "true" {
						ui, err := (&http.Client{Timeout: 30 * time.Second}).Get(loginURL.String())
						if err != nil {
							t.Fatal(err)
						}
						html, _ := io.ReadAll(io.LimitReader(ui.Body, 2<<20))
						ui.Body.Close()
						if ui.StatusCode != 200 || !strings.Contains(string(html), "<html") {
							t.Fatalf("real Zitadel Login V2 page: %d", ui.StatusCode)
						}
					}
					requestID := loginURL.Query().Get("authRequest")
					if requestID == "" {
						t.Fatalf("missing Zitadel auth request at %s", loginURL.Path)
					}
					var session struct {
						ID    string `json:"sessionId"`
						Token string `json:"sessionToken"`
					}
					login.must(t, "POST", "/v2/sessions", map[string]any{"checks": map[string]any{"user": map[string]string{"userId": users[tc.name].ID}, "password": map[string]string{"password": password}}}, &session)
					defer func() {
						if err := login.call("DELETE", "/v2/sessions/"+session.ID, map[string]string{"sessionToken": session.Token}, nil); err != nil {
							t.Error(err)
						}
					}()
					var callback struct {
						URL string `json:"callbackUrl"`
					}
					login.must(t, "POST", "/v2/oidc/auth_requests/"+requestID, map[string]any{"session": map[string]string{"sessionId": session.ID, "sessionToken": session.Token}}, &callback)
					res = zitadelGet(t, browser, callback.URL)
					body, _ := io.ReadAll(res.Body)
					res.Body.Close()
					if res.StatusCode != tc.want {
						t.Fatalf("callback: got %d want %d: %s", res.StatusCode, tc.want, body)
					}
					if tc.want == 303 {
						res = zitadelGet(t, browser, app.URL+tc.path)
						body, _ = io.ReadAll(res.Body)
						res.Body.Close()
						if res.StatusCode != 200 || !strings.Contains(string(body), wantZitadelBody(tc.path)) {
							t.Fatalf("protected content: %d %s", res.StatusCode, body)
						}
						// The real subject claim can also grant this already-authenticated user.
						subjectPolicy, _ := json.Marshal(map[string]any{"version": 1, "auth": "oidc", "allow": map[string]any{"subjects": []string{users[tc.name].ID}}})
						deploy(string(subjectPolicy))
						res = zitadelGet(t, browser, app.URL+tc.path)
						res.Body.Close()
						if res.StatusCode != 200 {
							t.Fatalf("subject grant: %d", res.StatusCode)
						}
						// Removing grants in the S3 policy revokes the still-valid real-provider session.
						deploy(`{"version":1,"auth":"oidc","allow":{}}`)
						res = zitadelGet(t, browser, app.URL+tc.path)
						res.Body.Close()
						if res.StatusCode != 403 {
							t.Fatalf("revoked session: %d", res.StatusCode)
						}
						deploy(string(policy))
					}
				})
			}
		})
	}
}

type zitadelAPI struct {
	issuer, token string
	client        *http.Client
}

func (a zitadelAPI) call(method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, a.issuer+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("Zitadel %s %s: %d %s", method, path, res.StatusCode, data)
	}
	if output != nil {
		return json.NewDecoder(res.Body).Decode(output)
	}
	return nil
}
func (a zitadelAPI) must(t *testing.T, method, path string, input, output any) {
	t.Helper()
	if err := a.call(method, path, input, output); err != nil {
		t.Fatal(err)
	}
}
func zitadelGet(t *testing.T, client *http.Client, location string) *http.Response {
	t.Helper()
	res, err := client.Get(location)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantZitadelBody(path string) string {
	if path == "/docs/" {
		return "role-protected docs"
	}
	return "private Zitadel home"
}
