package s3site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

const readingListHost = "reading-list.oddship.net"

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
	if len(sites[0].Headers) != 0 || len(sites[1].Headers) != 0 {
		t.Fatalf("expected configs without headers to remain compatible, got %+v", sites)
	}
}

func TestLoadHostedSitesHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sites.json")
	if err := os.WriteFile(path, []byte(`{"sites":[{"hostname":"Example.COM","headers":[{"path":"/feed.xml","name":"access-control-allow-origin","value":"*"},{"pathPrefix":"/feeds/","name":"Cache-Control","value":"public, max-age=300"}]}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	sites, err := LoadHostedSites(path, "sites/")
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(sites))
	}
	if len(sites[0].Headers) != 2 {
		t.Fatalf("expected 2 headers, got %+v", sites[0].Headers)
	}
	if got := sites[0].Headers[0]; got.Path != "/feed.xml" || got.Name != "Access-Control-Allow-Origin" || got.Value != "*" {
		t.Fatalf("unexpected exact header rule: %+v", got)
	}
	if got := sites[0].Headers[1]; got.PathPrefix != "/feeds/" || got.Name != "Cache-Control" || got.Value != "public, max-age=300" {
		t.Fatalf("unexpected prefix header rule: %+v", got)
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

func TestHandlerServesSite404ForMissingPath(t *testing.T) {
	const body = "<!doctype html><h1>Custom not found</h1>"
	for _, storage := range []StorageMode{StorageMemory, StorageDisk} {
		t.Run(fmt.Sprint(storage), func(t *testing.T) {
			sm := NewSiteManager(SiteManagerConfig{Storage: storage, DataDir: t.TempDir()})
			files := map[string]string{"index.html": "home", "404.html": body, "docs/index.html": "docs"}
			var siteFS fs.FS
			if storage == StorageDisk {
				site, err := sm.extractToDisk("example.com", newTarReader(t, files))
				if err != nil {
					t.Fatal(err)
				}
				siteFS = site.fs
			} else {
				mfs := newMemFS()
				for name, content := range files {
					mfs.Add(name, []byte(content))
				}
				siteFS = mfs
			}
			sm.sites["example.com"] = &site{hostname: "example.com", fs: siteFS}
			handler := Handler(sm, nil, "")
			for _, tc := range []struct {
				name, method, target, host, header, value string
				status                                    int
				body                                      string
			}{
				{name: "missing", target: "/missing", status: 404, body: body},
				{name: "nested", target: "/docs/missing", status: 404, body: body},
				{name: "clean path", target: "//docs/../missing", status: 404, body: body},
				{name: "head", method: "HEAD", target: "/missing", status: 404},
				{name: "range", target: "/missing", header: "Range", value: "bytes=0-3", status: 404, body: body},
				{name: "conditional", target: "/missing", header: "If-None-Match", value: "*", status: 404, body: body},
				{name: "home", target: "/", status: 200, body: "home"},
				{name: "direct fallback", target: "/404.html", status: 200, body: body},
				{name: "directory", target: "/docs/", status: 200, body: "docs"},
				{name: "unknown host", target: "/missing", host: "unknown.com", status: 404, body: "site not found: unknown.com\n"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					method := tc.method
					if method == "" {
						method = "GET"
					}
					req := httptest.NewRequest(method, "http://example.com"+tc.target, nil)
					if tc.host != "" {
						req.Host = tc.host
					}
					if tc.header != "" {
						req.Header.Set(tc.header, tc.value)
					}
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, req)
					resp := w.Result()
					defer resp.Body.Close()
					if resp.StatusCode != tc.status {
						t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
					}
					if got := w.Body.String(); got != tc.body {
						t.Fatalf("body = %q, want %q", got, tc.body)
					}
					if tc.status == 404 && tc.host == "" {
						if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
							t.Errorf("Content-Type = %q", got)
						}
						if got := resp.ContentLength; got != int64(len(body)) {
							t.Errorf("Content-Length = %d, want %d", got, len(body))
						}
						if got := resp.Header.Get("Content-Range"); got != "" {
							t.Errorf("unexpected Content-Range %q", got)
						}
					}
				})
			}
		})
	}
}

func TestHandlerMissingFallback(t *testing.T) {
	for _, files := range []map[string]string{
		{"index.html": "home"},
		{"index.html": "home", "404.html/index.html": "a directory is not an error page"},
	} {
		handler := newHostedTestHandler(t, "example.com", nil, files, "")
		req := httptest.NewRequest(http.MethodGet, "http://example.com/missing", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
			t.Fatalf("expected standard 404, got %d %q", w.Code, w.Body.String())
		}
	}
}

func TestHandlerAppliesExactHostedHeader(t *testing.T) {
	handler := newHostedTestHandler(t, readingListHost, []HeaderRule{{
		Path:  "/feed.xml",
		Name:  "Access-Control-Allow-Origin",
		Value: "*",
	}}, map[string]string{
		"feed.xml": "<rss></rss>",
	}, "")

	req := httptest.NewRequest(http.MethodGet, "/feed.xml", nil)
	req.Host = readingListHost
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected CORS header, got %q", got)
	}
}

func TestHandlerDoesNotApplyHostedHeaderToUnrelatedPath(t *testing.T) {
	handler := newHostedTestHandler(t, readingListHost, []HeaderRule{{
		Path:  "/feed.xml",
		Name:  "Access-Control-Allow-Origin",
		Value: "*",
	}}, map[string]string{
		"about.html": "ok",
	}, "")

	req := httptest.NewRequest(http.MethodGet, "/about.html", nil)
	req.Host = readingListHost
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS header, got %q", got)
	}
}

func TestHandlerAppliesMultipleHostedHeaderRules(t *testing.T) {
	handler := newHostedTestHandler(t, readingListHost, []HeaderRule{
		{Path: "/feed.xml", Name: "Access-Control-Allow-Origin", Value: "*"},
		{Path: "/feed.xml", Name: "Access-Control-Allow-Methods", Value: "GET"},
		{PathPrefix: "/feeds/", Name: "Cache-Control", Value: "public, max-age=300"},
	}, map[string]string{
		"feed.xml":       "<rss></rss>",
		"feeds/atom.xml": "<feed></feed>",
	}, "")

	req := httptest.NewRequest(http.MethodGet, "/feed.xml", nil)
	req.Host = readingListHost
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected CORS origin header, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET" {
		t.Fatalf("expected CORS methods header, got %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/feeds/atom.xml", nil)
	req.Host = readingListHost
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("expected cache header, got %q", got)
	}
}

func TestHandlerDoesNotApplyHostedHeadersToAdminHost(t *testing.T) {
	const adminHost = "admin.example.com"
	handler := newHostedTestHandler(t, adminHost, []HeaderRule{{
		Path:  "/",
		Name:  "X-Feed-Header",
		Value: "site",
	}}, map[string]string{
		"index.html": "ok",
	}, adminHost)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = adminHost
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("X-Feed-Header"); got != "" {
		t.Fatalf("expected no hosted header on admin host, got %q", got)
	}
}

func newHostedTestHandler(t *testing.T, hostname string, headers []HeaderRule, files map[string]string, adminHost string) http.Handler {
	t.Helper()
	sm := NewSiteManager(SiteManagerConfig{
		S3:     setupDummyS3(),
		Bucket: "bucket",
		HostedSites: []HostedSite{{
			Hostname: hostname,
			Headers:  headers,
		}},
	})
	if err := sm.Validate(); err != nil {
		t.Fatal(err)
	}
	mfs := newMemFS()
	for name, content := range files {
		mfs.Add(name, []byte(content))
	}
	sm.sites[hostname] = &site{hostname: hostname, fs: mfs}
	return Handler(sm, nil, adminHost)
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
