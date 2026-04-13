package s3site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

func TestLoadHostedSites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sites.json")
	if err := os.WriteFile(path, []byte(`{"sites":[{"hostname":"Example.COM"},{"hostname":"docs.example.com","key":"custom/docs.tar.gz"}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	sites, err := LoadHostedSites(path, "sites/")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("expected 2 sites, got %d", len(sites))
	}
	if sites[0].Hostname != "example.com" || sites[0].Key != "sites/example.com.tar.gz" {
		t.Fatalf("unexpected first site: %+v", sites[0])
	}
	if sites[1].Hostname != "docs.example.com" || sites[1].Key != "custom/docs.tar.gz" {
		t.Fatalf("unexpected second site: %+v", sites[1])
	}
}

func TestLoadHostedSitesRejectsDuplicateHostnames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sites.json")
	if err := os.WriteFile(path, []byte(`{"sites":[{"hostname":"Example.COM"},{"hostname":"example.com"}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadHostedSites(path, "sites/")
	if err == nil {
		t.Fatal("expected duplicate hostname error")
	}
}

func TestArchiveToHostnameRejectsInvalidNames(t *testing.T) {
	if got := archiveToHostname("sites/foo/bar.tar.gz", "sites/"); got != "" {
		t.Fatalf("expected empty hostname for nested key, got %q", got)
	}
	if got := archiveToHostname("sites/../evil.tar.gz", "sites/"); got != "" {
		t.Fatalf("expected empty hostname for traversal key, got %q", got)
	}
}

func TestHandlerNormalizesHostHeader(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{})
	mfs := newMemFS()
	mfs.Add("index.html", []byte("ok"))
	sm.sites["example.com"] = &site{hostname: "example.com", fs: mfs}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "Example.COM:8080"
	w := httptest.NewRecorder()

	Handler(sm, nil, "").ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); body != "ok" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestNewSiteManagerValidation(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		Interval:        -1 * time.Second,
		MaxArchiveFiles: -1,
		MaxArchiveBytes: -1,
	})
	if err := sm.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestNewSiteManagerNormalizesHostedSites(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		S3:     setupDummyS3(),
		Bucket: "bucket",
		Prefix: "sites/",
		HostedSites: []HostedSite{
			{Hostname: "Example.COM"},
		},
	})
	if err := sm.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.declaredSites["example.com"]; !ok {
		t.Fatalf("expected normalized hosted site, got %#v", sm.declaredSites)
	}
	if got := sm.declaredSites["example.com"].Key; got != "sites/example.com.tar.gz" {
		t.Fatalf("unexpected derived key %q", got)
	}
}

func TestDiskActivationRetainsPreviousVersion(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		Storage: StorageDisk,
		DataDir: t.TempDir(),
	})

	siteA, err := sm.extractToDisk("example.com", newTarReader(t, map[string]string{"index.html": "v1"}))
	if err != nil {
		t.Fatal(err)
	}
	cleanup := sm.activateSite("example.com", siteA)
	if len(cleanup) != 0 {
		t.Fatalf("expected no cleanup paths on first activation, got %v", cleanup)
	}

	siteB, err := sm.extractToDisk("example.com", newTarReader(t, map[string]string{"index.html": "v2"}))
	if err != nil {
		t.Fatal(err)
	}
	cleanup = sm.activateSite("example.com", siteB)
	if len(cleanup) != 0 {
		t.Fatalf("expected previous version to be retained for one generation, got cleanup %v", cleanup)
	}
	if len(siteB.retainedDirs) != 1 || siteB.retainedDirs[0] != siteA.dir {
		t.Fatalf("expected retained previous dir %q, got %v", siteA.dir, siteB.retainedDirs)
	}
	if _, err := os.Stat(siteA.dir); err != nil {
		t.Fatalf("expected retained dir to still exist: %v", err)
	}

	siteC, err := sm.extractToDisk("example.com", newTarReader(t, map[string]string{"index.html": "v3"}))
	if err != nil {
		t.Fatal(err)
	}
	cleanup = sm.activateSite("example.com", siteC)
	if len(cleanup) != 1 || cleanup[0] != siteA.dir {
		t.Fatalf("expected oldest dir %q to be eligible for cleanup, got %v", siteA.dir, cleanup)
	}
	cleanupOldDirs(cleanup)
	if _, err := os.Stat(siteA.dir); !os.IsNotExist(err) {
		t.Fatalf("expected oldest retained dir to be removed, got %v", err)
	}
	if _, err := os.Stat(siteB.dir); err != nil {
		t.Fatalf("expected previous dir to remain, got %v", err)
	}
	if _, err := os.Stat(siteC.dir); err != nil {
		t.Fatalf("expected current dir to remain, got %v", err)
	}
}

func TestCleanTarPathRejectsTraversal(t *testing.T) {
	if _, err := cleanTarPath("../evil.txt"); err == nil {
		t.Fatal("expected traversal path to fail")
	}
	if _, err := cleanTarPath("./index.html"); err != nil {
		t.Fatalf("expected clean path to pass, got %v", err)
	}
}

func setupDummyS3() *simples3.S3 {
	return simples3.New("us-east-1", "AKID", "SECRET")
}

func newTarReader(t *testing.T, files map[string]string) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	gr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gr.Close() })
	return tar.NewReader(gr)
}

func TestRefreshHostsRequiresHostedMode(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{})
	if err := sm.RefreshHosts([]string{"example.com"}); err == nil {
		t.Fatal("expected refresh to fail outside hosted mode")
	}
}

func TestObjectKeyForHostUsesHostedOverride(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		S3:     setupDummyS3(),
		Bucket: "bucket",
		Prefix: "sites/",
		HostedSites: []HostedSite{
			{Hostname: "example.com", Key: "custom/example.tar.gz"},
		},
	})
	if err := sm.Validate(); err != nil {
		t.Fatal(err)
	}

	got, err := sm.objectKeyForHost("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "custom/example.tar.gz" {
		t.Fatalf("unexpected key %q", got)
	}
}

func TestObjectKeyForHostRejectsUndeclaredHostedHost(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		S3:     setupDummyS3(),
		Bucket: "bucket",
		Prefix: "sites/",
		HostedSites: []HostedSite{
			{Hostname: "example.com", Key: "custom/example.tar.gz"},
		},
	})
	if err := sm.Validate(); err != nil {
		t.Fatal(err)
	}

	if _, err := sm.objectKeyForHost("other.example.com"); err == nil {
		t.Fatal("expected undeclared hosted host to fail")
	}
}

func TestObjectKeyForHostDiscoveryDefaults(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{Prefix: "sites/"})
	got, err := sm.objectKeyForHost("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sites/example.com.tar.gz" {
		t.Fatalf("unexpected key %q", got)
	}
}

func TestCleanTarPathRoundTrip(t *testing.T) {
	name, err := cleanTarPath("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if name != "assets/app.js" {
		t.Fatalf("unexpected cleaned path %q", name)
	}
}

func TestExtractToMemoryRejectsTooManyFiles(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{MaxArchiveFiles: 1})
	_, err := sm.extractToMemory("example.com", newTarReader(t, map[string]string{
		"a.txt": "a",
		"b.txt": "b",
	}))
	if err == nil {
		t.Fatal("expected file limit error")
	}
}

func TestExtractToDiskRejectsOversizeArchive(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		Storage:         StorageDisk,
		DataDir:         t.TempDir(),
		MaxArchiveBytes: 1,
	})
	_, err := sm.extractToDisk("example.com", newTarReader(t, map[string]string{"index.html": "too-big"}))
	if err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestDiskExtractedSiteServesFiles(t *testing.T) {
	sm := NewSiteManager(SiteManagerConfig{
		Storage: StorageDisk,
		DataDir: t.TempDir(),
	})
	s, err := sm.extractToDisk("example.com", newTarReader(t, map[string]string{"index.html": "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.fs.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("unexpected file contents %q", data)
	}
}
