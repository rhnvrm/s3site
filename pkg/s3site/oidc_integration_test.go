package s3site

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

func TestOIDCS3Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a local S3-compatible service; see examples/oidc/README.md")
	}
	for _, mode := range []StorageMode{StorageMemory, StorageDisk} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			f := newLoginFixture(t)
			s3, bucket := setupS3(t)
			key := fmt.Sprintf("oidc-integration-%d/site.tar.gz", time.Now().UnixNano())
			t.Cleanup(func() { _ = s3.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: key}) })
			upload := func(policy, content string) {
				uploadArchive(t, s3, bucket, key, makeTarGz(t, map[string]string{accessPolicyPath: policy, "index.html": content, "asset.txt": "private asset"}))
			}
			upload(testPolicy, "S3 private home")
			sm := NewSiteManager(SiteManagerConfig{S3: s3, Bucket: bucket, Storage: mode, DataDir: t.TempDir(), HostedSites: []HostedSite{{Hostname: f.host, Key: key}}})
			if err := sm.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := sm.syncOnce(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if s := sm.getSiteSnapshot(f.host); s != nil {
					s.cleanup()
				}
			})
			f.sm = sm
			f.app.Config.Handler = HandlerWithOIDC(sm, nil, "", f.auth)
			res := f.authenticate(t, f.begin(t, "/asset.txt"), "alice")
			res.Body.Close()
			if res.StatusCode != 303 {
				t.Fatalf("login: %d", res.StatusCode)
			}
			check := func(want int, body string) {
				t.Helper()
				res, err := f.client.Get(f.app.URL + "/")
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				data, _ := io.ReadAll(res.Body)
				if res.StatusCode != want || string(data) != body {
					t.Fatalf("got %d %q", res.StatusCode, data)
				}
			}
			check(http.StatusOK, "S3 private home")
			upload(`{"version":1,"auth":"oidc","allow":{"typo":true}}`, "must not activate")
			if err := sm.RefreshHosts([]string{f.host}); err == nil {
				t.Fatal("invalid deployment accepted")
			}
			check(http.StatusOK, "S3 private home")
			upload(`{"version":1,"auth":"oidc","allow":{}}`, "revoked")
			if err := sm.RefreshHosts([]string{f.host}); err != nil {
				t.Fatal(err)
			}
			check(http.StatusForbidden, "Forbidden\n")
		})
	}
}
