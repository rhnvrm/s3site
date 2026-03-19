package s3site

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rhnvrm/simples3"
)

func setupS3(t *testing.T) (*simples3.S3, string) {
	t.Helper()

	endpoint := os.Getenv("AWS_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:9000"
	}
	region := os.Getenv("AWS_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	accessKey := os.Getenv("AWS_S3_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("AWS_S3_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		bucket = "testbucket"
	}

	s3 := simples3.New(region, accessKey, secretKey)
	s3.SetEndpoint(endpoint)
	return s3, bucket
}

// makeTarGz creates a tar.gz archive from a map of filename -> content.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}

	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func uploadArchive(t *testing.T, s3Client *simples3.S3, bucket, key string, data []byte) {
	t.Helper()
	_, err := s3Client.FilePut(simples3.UploadInput{
		Bucket:      bucket,
		ObjectKey:   key,
		ContentType: "application/gzip",
		Body:        bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("upload %s: %v", key, err)
	}
}

func TestMemFS(t *testing.T) {
	mfs := newMemFS()
	mfs.Add("index.html", []byte("<h1>Home</h1>"))
	mfs.Add("css/style.css", []byte("body { color: red; }"))
	mfs.Add("js/app.js", []byte("console.log('hi')"))
	mfs.Add("about/index.html", []byte("<h1>About</h1>"))

	// Validate with Go's fstest.
	if err := fstest.TestFS(mfs, "index.html", "css/style.css", "js/app.js", "about/index.html"); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveToHostname(t *testing.T) {
	tests := []struct {
		key, prefix, want string
	}{
		{"foo.example.com.tar.gz", "", "foo.example.com"},
		{"sites/foo.example.com.tar.gz", "sites/", "foo.example.com"},
		{"bar.tar.gz", "", "bar"},
		{"not-a-tarball.txt", "", ""},
	}
	for _, tt := range tests {
		got := archiveToHostname(tt.key, tt.prefix)
		if got != tt.want {
			t.Errorf("archiveToHostname(%q, %q) = %q, want %q", tt.key, tt.prefix, got, tt.want)
		}
	}
}

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	s3Client, bucket := setupS3(t)

	// Use a unique prefix per test run to avoid collisions with demo data.
	prefix := fmt.Sprintf("test-%d/", time.Now().UnixNano())

	// Create two site archives.
	siteA := makeTarGz(t, map[string]string{
		"index.html":    "<h1>Site A</h1>",
		"about.html":    "<h1>About A</h1>",
		"css/style.css": "body { background: blue; }",
	})
	siteB := makeTarGz(t, map[string]string{
		"index.html": "<h1>Site B</h1>",
		"data.json":  `{"version": 1}`,
	})

	// Upload to S3.
	uploadArchive(t, s3Client, bucket, prefix+"alpha.example.com.tar.gz", siteA)
	uploadArchive(t, s3Client, bucket, prefix+"beta.example.com.tar.gz", siteB)
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: prefix + "alpha.example.com.tar.gz"})
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: prefix + "beta.example.com.tar.gz"})
	})

	// Create site manager.
	sm := NewSiteManager(SiteManagerConfig{
		S3:       s3Client,
		Bucket:   bucket,
		Prefix:   prefix,
		Interval: 500 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sm.Start(ctx)

	// Wait for initial sync.
	time.Sleep(1 * time.Second)

	// Check loaded sites.
	hosts := sm.Hostnames()
	if len(hosts) != 2 {
		t.Fatalf("expected 2 sites, got %d: %v", len(hosts), hosts)
	}
	t.Logf("loaded sites: %v", hosts)

	// Set up HTTP test server.
	handler := Handler(sm, nil, "")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Helper to make requests with a Host header.
	get := func(host, path string, wantStatus int) string {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s%s: %v", host, path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != wantStatus {
			t.Errorf("GET %s%s: status=%d, want %d, body=%q",
				host, path, resp.StatusCode, wantStatus, body)
		}
		return string(body)
	}

	// --- Test serving ---

	// Site A.
	body := get("alpha.example.com", "/index.html", 200)
	if body != "<h1>Site A</h1>" {
		t.Errorf("site A index = %q", body)
	}

	body = get("alpha.example.com", "/about.html", 200)
	if body != "<h1>About A</h1>" {
		t.Errorf("site A about = %q", body)
	}

	body = get("alpha.example.com", "/css/style.css", 200)
	if body != "body { background: blue; }" {
		t.Errorf("site A css = %q", body)
	}

	// Site B.
	body = get("beta.example.com", "/index.html", 200)
	if body != "<h1>Site B</h1>" {
		t.Errorf("site B index = %q", body)
	}

	body = get("beta.example.com", "/data.json", 200)
	if body != `{"version": 1}` {
		t.Errorf("site B data = %q", body)
	}

	// Unknown host.
	get("unknown.example.com", "/", 404)

	// Non-existent file on valid host.
	get("alpha.example.com", "/nope.txt", 404)

	// --- Test hot reload ---

	// Update site A.
	siteAv2 := makeTarGz(t, map[string]string{
		"index.html": "<h1>Site A v2</h1>",
	})
	uploadArchive(t, s3Client, bucket, prefix+"alpha.example.com.tar.gz", siteAv2)

	// Wait for poll.
	time.Sleep(2 * time.Second)

	body = get("alpha.example.com", "/index.html", 200)
	if body != "<h1>Site A v2</h1>" {
		t.Errorf("after update, site A index = %q", body)
	}

	// Old files should be gone after re-extract.
	get("alpha.example.com", "/about.html", 404)
	t.Log("hot reload verified")

	// --- Test site removal ---

	s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: prefix + "beta.example.com.tar.gz"})
	time.Sleep(2 * time.Second)

	get("beta.example.com", "/index.html", 404)
	t.Log("site removal verified")

	cancel()
}
