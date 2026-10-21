package s3site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

func extractPolicyTestSite(t *testing.T, mode StorageMode, files map[string]string) (*site, error) {
	t.Helper()
	sm := NewSiteManager(SiteManagerConfig{Storage: mode, DataDir: t.TempDir()})
	gz, err := gzip.NewReader(bytes.NewReader(makeTarGz(t, files)))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	if mode == StorageDisk {
		return sm.extractToDisk("example.com", tar.NewReader(gz))
	}
	return sm.extractToMemory("example.com", tar.NewReader(gz))
}
func TestAccessPolicyExtraction(t *testing.T) {
	for _, mode := range []StorageMode{StorageMemory, StorageDisk} {
		t.Run(map[StorageMode]string{StorageMemory: "memory", StorageDisk: "disk"}[mode], func(t *testing.T) {
			for _, policy := range []string{testPolicy, `{"version":1,"auth":"oidc","allow":{}}`, `{"version":1,"auth":"oidc","allow":{"subjects":["stable-id"]}}`} {
				s, err := extractPolicyTestSite(t, mode, map[string]string{accessPolicyPath: policy, "visible.txt": "ok"})
				if err != nil {
					t.Fatal(err)
				}
				defer s.cleanup()
				if s.policy == nil {
					t.Fatal("policy missing")
				}
				if _, err := fs.ReadFile(s.fs, accessPolicyPath); err == nil {
					t.Fatal("policy publicly readable")
				}
				for _, path := range []string{"/", "/.s3site/access.json", "//.s3site/access.json", "/x/../.s3site/access.json", "/%2es3site/access.json"} {
					w := httptest.NewRecorder()
					serveSite(s.fs, w, httptest.NewRequest("GET", path, nil))
					if strings.Contains(w.Body.String(), ".s3site") || strings.Contains(w.Body.String(), policy) {
						t.Fatalf("metadata leaked at %s: %s", path, w.Body.String())
					}
				}
			}
			for _, policy := range []string{`{`, `null`, `{}`, `{"version":2,"auth":"oidc"}`, `{"version":1,"auth":"other"}`, `{"version":1,"auth":"oidc","typo":true}`, testPolicy + `{}`, `{"version":1,"auth":"oidc","allow":{"emails":["bad"]}}`, strings.Repeat(" ", 65537)} {
				if s, err := extractPolicyTestSite(t, mode, map[string]string{accessPolicyPath: policy}); err == nil {
					s.cleanup()
					t.Fatalf("invalid policy accepted: %.80s", policy)
				}
			}
			if _, err := extractPolicyTestSite(t, mode, map[string]string{accessPolicyPath + "/nested": "oops"}); err == nil {
				t.Fatal("directory policy accepted")
			}
			s, err := extractPolicyTestSite(t, mode, map[string]string{"index.html": "public"})
			if err != nil || s.policy != nil {
				t.Fatalf("public compatibility: %v", err)
			}
		})
	}
}
func TestSubjectAllowlist(t *testing.T) {
	p := &AccessPolicy{Allow: &Allowlist{}}
	p.Allow.Subjects = []string{"alice-id"}
	if !p.permits(session{Subject: "alice-id"}) || p.permits(session{Subject: "bob-id", Email: "alice@example.com", EmailVerified: true}) {
		t.Fatal("subject allowlist mismatch")
	}
}

func TestRuleMatching(t *testing.T) {
	p := &AccessPolicy{Version: 1, Auth: "oidc", Rules: []AccessRule{
		{Path: "/docs/secret.html", Allow: &Allowlist{Roles: []string{"admin"}}},
		{PathPrefix: "/docs", Public: true},
	}}
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/docs", "/docs/", "/docs/readme.html"} {
		if !p.match(path).public() {
			t.Fatalf("public path denied %s", path)
		}
	}
	for _, path := range []string{"/docs-other", "/other", "/docs/../other", "//other", "/docs//../other"} {
		if !p.match(path).denied {
			t.Fatalf("prefix escaped: %s", path)
		}
	}
	if p.match("/docs/secret.html").permits(session{}) || !p.match("/docs/secret.html").permits(session{Roles: []string{"admin"}}) {
		t.Fatal("first matching rule ignored")
	}
	// Public aliases cannot expose a protected index or error representation.
	mfs := newMemFS()
	mfs.Add("docs/index.html", []byte("secret"))
	mfs.Add("404.html", []byte("secret error"))
	p.Rules = append([]AccessRule{{Path: "/docs/index.html", Allow: &Allowlist{Roles: []string{"admin"}}}, {Path: "/404.html", Allow: &Allowlist{Roles: []string{"admin"}}}}, p.Rules...)
	for _, path := range []string{"/docs/", "/docs/missing"} {
		if p.decision(mfs, path).permits(session{}) {
			t.Fatalf("alias exposed private content: %s", path)
		}
	}
	for _, rule := range []AccessRule{{Public: true}, {Path: "relative", Public: true}, {Path: "/a/../b", Public: true}, {Path: "/a", PathPrefix: "/a", Public: true}, {Path: "/a", Public: true, Allow: &Allowlist{}}, {Path: "/a"}} {
		p.Rules = []AccessRule{rule}
		if p.validate() == nil {
			t.Fatalf("invalid rule accepted: %+v", rule)
		}
	}
}

func TestMetadataWrapperPreservesDiskRangeRequests(t *testing.T) {
	s, err := extractPolicyTestSite(t, StorageDisk, map[string]string{"file.txt": "abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.cleanup()
	r := httptest.NewRequest("GET", "/file.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	serveSite(s.fs, w, r)
	if w.Code != 206 || w.Body.String() != "bcd" {
		t.Fatalf("range regressed: %d %q", w.Code, w.Body.String())
	}
}
