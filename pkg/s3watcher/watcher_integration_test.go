package s3watcher

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

// setupIntegrationS3 creates a simples3 client pointing at the local MinIO.
// Skips the test if AWS_S3_ENDPOINT is not set.
func setupIntegrationS3(t *testing.T) (*simples3.S3, string) {
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

// putObject is a helper to upload a string as an S3 object.
func putObject(t *testing.T, s3Client *simples3.S3, bucket, key, content string) {
	t.Helper()
	_, err := s3Client.FilePut(simples3.UploadInput{
		Bucket:      bucket,
		ObjectKey:   key,
		ContentType: "text/plain",
		Body:        bytes.NewReader([]byte(content)),
	})
	if err != nil {
		t.Fatalf("failed to put object %s/%s: %v", bucket, key, err)
	}
}

func TestWatcherIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s3Client, bucket := setupIntegrationS3(t)

	// Upload initial files.
	putObject(t, s3Client, bucket, "watcher-test/instruments.csv", "id,name\n1,INFY\n2,TCS\n")
	putObject(t, s3Client, bucket, "watcher-test/holidays.json", `["2026-01-26","2026-08-15"]`)

	// Cleanup.
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/instruments.csv"})
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/holidays.json"})
	})

	// Create watcher.
	loadOnStart := true
	w, err := New(Config{
		S3:           s3Client,
		PollInterval: 500 * time.Millisecond, // fast polling for tests
		Files: []FileEntry{
			{Name: "instruments", Bucket: bucket, Key: "watcher-test/instruments.csv"},
			{Name: "holidays", Bucket: bucket, Key: "watcher-test/holidays.json"},
		},
		LoadOnStart: &loadOnStart,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Track update events.
	var mu sync.Mutex
	events := make(map[string][]UpdateEvent)
	w.OnUpdate(func(e UpdateEvent) {
		mu.Lock()
		events[e.Name] = append(events[e.Name], e)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start watcher in background.
	go w.Start(ctx)

	// Wait for initial load.
	time.Sleep(1 * time.Second)

	// Verify initial load.
	data, etag := w.Get("instruments")
	if data == nil {
		t.Fatal("instruments not loaded")
	}
	if string(data) != "id,name\n1,INFY\n2,TCS\n" {
		t.Errorf("unexpected instruments content: %q", string(data))
	}
	if etag == "" {
		t.Error("instruments etag should not be empty")
	}
	t.Logf("initial instruments etag: %s, size: %d", etag, len(data))

	data, _ = w.Get("holidays")
	if data == nil {
		t.Fatal("holidays not loaded")
	}
	if string(data) != `["2026-01-26","2026-08-15"]` {
		t.Errorf("unexpected holidays content: %q", string(data))
	}

	// Verify initial load triggered callbacks.
	mu.Lock()
	if len(events["instruments"]) != 1 {
		t.Errorf("expected 1 initial instruments event, got %d", len(events["instruments"]))
	}
	if len(events["holidays"]) != 1 {
		t.Errorf("expected 1 initial holidays event, got %d", len(events["holidays"]))
	}
	mu.Unlock()

	// Now update one file and verify the watcher picks it up.
	t.Log("uploading updated instruments file...")
	putObject(t, s3Client, bucket, "watcher-test/instruments.csv", "id,name\n1,INFY\n2,TCS\n3,RELIANCE\n")

	// Wait for poll to detect change.
	time.Sleep(2 * time.Second)

	// Verify updated content.
	data, newEtag := w.Get("instruments")
	if string(data) != "id,name\n1,INFY\n2,TCS\n3,RELIANCE\n" {
		t.Errorf("instruments not updated, got: %q", string(data))
	}
	if newEtag == etag {
		t.Error("etag should have changed after update")
	}
	t.Logf("updated instruments etag: %s, size: %d", newEtag, len(data))

	// Verify update callback fired.
	mu.Lock()
	if len(events["instruments"]) != 2 {
		t.Errorf("expected 2 instruments events (initial + update), got %d", len(events["instruments"]))
	}
	// Holidays should still be at 1 (unchanged).
	if len(events["holidays"]) != 1 {
		t.Errorf("holidays should still have 1 event, got %d", len(events["holidays"]))
	}
	mu.Unlock()

	// Verify no-change polls don't trigger callbacks.
	time.Sleep(2 * time.Second)
	mu.Lock()
	if len(events["instruments"]) != 2 {
		t.Errorf("no-change poll should not trigger callback, got %d events", len(events["instruments"]))
	}
	mu.Unlock()

	cancel()
}

func TestWatcherIntegration_LazyLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s3Client, bucket := setupIntegrationS3(t)

	putObject(t, s3Client, bucket, "watcher-test/lazy-a.txt", "lazy-content-a")
	putObject(t, s3Client, bucket, "watcher-test/lazy-b.txt", "lazy-content-b")
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/lazy-a.txt"})
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/lazy-b.txt"})
	})

	// LoadOnStart=false so nothing is fetched eagerly.
	loadOnStart := false
	var mu sync.Mutex
	events := make(map[string]int)

	w, err := New(Config{
		S3:           s3Client,
		PollInterval: 500 * time.Millisecond,
		Files: []FileEntry{
			{Name: "lazy-a", Bucket: bucket, Key: "watcher-test/lazy-a.txt"},
			{Name: "lazy-b", Bucket: bucket, Key: "watcher-test/lazy-b.txt"},
		},
		LoadOnStart: &loadOnStart,
	})
	if err != nil {
		t.Fatal(err)
	}

	w.OnUpdate(func(e UpdateEvent) {
		mu.Lock()
		events[e.Name]++
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	// Before any Get(), files should not be fetched.
	// Give poll loop a tick to confirm it skips unaccessed files.
	time.Sleep(800 * time.Millisecond)
	mu.Lock()
	if events["lazy-a"] != 0 || events["lazy-b"] != 0 {
		t.Errorf("unaccessed files should not be polled, got events: %v", events)
	}
	mu.Unlock()

	// First Get() triggers lazy load for lazy-a only.
	data, etag := w.Get("lazy-a")
	if data == nil {
		t.Fatal("lazy-a should be loaded on first Get()")
	}
	if string(data) != "lazy-content-a" {
		t.Errorf("lazy-a content = %q, want %q", data, "lazy-content-a")
	}
	if etag == "" {
		t.Error("lazy-a etag should not be empty")
	}
	t.Logf("lazy-a loaded on demand: %d bytes, etag=%s", len(data), etag)

	// lazy-b should still be nil (never accessed).
	rawB := w.states["lazy-b"]
	rawB.mu.RLock()
	bAccessed := rawB.accessed
	rawB.mu.RUnlock()
	if bAccessed {
		t.Error("lazy-b should not be accessed yet")
	}

	// Callback should have fired for lazy-a.
	mu.Lock()
	if events["lazy-a"] != 1 {
		t.Errorf("expected 1 lazy-a event from lazy load, got %d", events["lazy-a"])
	}
	if events["lazy-b"] != 0 {
		t.Errorf("expected 0 lazy-b events, got %d", events["lazy-b"])
	}
	mu.Unlock()

	// Now update lazy-a on S3 and verify the poll loop picks it up
	// (because it's now accessed).
	putObject(t, s3Client, bucket, "watcher-test/lazy-a.txt", "lazy-content-a-v2")
	time.Sleep(1500 * time.Millisecond)

	data, _ = w.Get("lazy-a")
	if string(data) != "lazy-content-a-v2" {
		t.Errorf("lazy-a not updated by poll, got %q", data)
	}

	mu.Lock()
	if events["lazy-a"] != 2 {
		t.Errorf("expected 2 lazy-a events (lazy load + poll update), got %d", events["lazy-a"])
	}
	// lazy-b should still be 0 since nobody accessed it.
	if events["lazy-b"] != 0 {
		t.Errorf("lazy-b should still have 0 events, got %d", events["lazy-b"])
	}
	mu.Unlock()

	// Now access lazy-b via FS interface.
	fsys := w.FS()
	fsData, err := fsys.ReadFile("lazy-b")
	if err != nil {
		t.Fatal(err)
	}
	if string(fsData) != "lazy-content-b" {
		t.Errorf("lazy-b via FS = %q, want %q", fsData, "lazy-content-b")
	}
	t.Log("lazy-b loaded on demand via FS")

	cancel()
}

func TestWatcherIntegration_ClearAndRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s3Client, bucket := setupIntegrationS3(t)

	putObject(t, s3Client, bucket, "watcher-test/cache-a.txt", "version-1")
	putObject(t, s3Client, bucket, "watcher-test/cache-b.txt", "other-file")
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/cache-a.txt"})
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/cache-b.txt"})
	})

	loadOnStart := true
	w, err := New(Config{
		S3:           s3Client,
		PollInterval: 10 * time.Minute, // long interval - we test manual clear/refresh
		Files: []FileEntry{
			{Name: "cache-a", Bucket: bucket, Key: "watcher-test/cache-a.txt"},
			{Name: "cache-b", Bucket: bucket, Key: "watcher-test/cache-b.txt"},
		},
		LoadOnStart: &loadOnStart,
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	events := make(map[string]int)
	w.OnUpdate(func(e UpdateEvent) {
		mu.Lock()
		events[e.Name]++
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)
	time.Sleep(200 * time.Millisecond) // let initial load complete

	// Verify initial state.
	data, _ := w.Get("cache-a")
	if string(data) != "version-1" {
		t.Fatalf("expected version-1, got %q", data)
	}

	// --- Test Clear ---
	w.Clear("cache-a")

	data, etag := w.Get("cache-a")
	// Get() triggers lazy reload after clear.
	if string(data) != "version-1" {
		t.Errorf("after clear + get, expected version-1 (re-fetched), got %q", data)
	}
	t.Logf("after clear + re-fetch: etag=%s", etag)

	// --- Test Clear then update on S3, then Get ---
	putObject(t, s3Client, bucket, "watcher-test/cache-a.txt", "version-2")
	w.Clear("cache-a")

	data, _ = w.Get("cache-a")
	if string(data) != "version-2" {
		t.Errorf("after S3 update + clear + get, expected version-2, got %q", data)
	}

	// --- Test ClearAll ---
	w.ClearAll()

	// Both files evicted - Get re-fetches.
	data, _ = w.Get("cache-a")
	if string(data) != "version-2" {
		t.Errorf("after ClearAll, cache-a should re-fetch, got %q", data)
	}
	data, _ = w.Get("cache-b")
	if string(data) != "other-file" {
		t.Errorf("after ClearAll, cache-b should re-fetch, got %q", data)
	}

	// --- Test Refresh ---
	putObject(t, s3Client, bucket, "watcher-test/cache-a.txt", "version-3")

	// Without refresh, cached data is still version-2 (poll interval is 10min).
	data, _ = w.Get("cache-a")
	if string(data) != "version-2" {
		t.Errorf("before refresh, should still have version-2, got %q", data)
	}

	// Refresh forces re-fetch.
	if err := w.Refresh("cache-a"); err != nil {
		t.Fatal(err)
	}

	data, _ = w.Get("cache-a")
	if string(data) != "version-3" {
		t.Errorf("after refresh, expected version-3, got %q", data)
	}

	// Refresh unknown file.
	if err := w.Refresh("nonexistent"); err == nil {
		t.Error("Refresh of unknown file should return error")
	}

	cancel()
}

func TestWatcherIntegration_LazyGetBeforeStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s3Client, bucket := setupIntegrationS3(t)

	putObject(t, s3Client, bucket, "watcher-test/noload.txt", "hello")
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/noload.txt"})
	})

	loadOnStart := false
	w, err := New(Config{
		S3:           s3Client,
		PollInterval: 500 * time.Millisecond,
		Files: []FileEntry{
			{Name: "noload", Bucket: bucket, Key: "watcher-test/noload.txt"},
		},
		LoadOnStart: &loadOnStart,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Even before Start(), Get() should lazy-load the file.
	data, etag := w.Get("noload")
	if data == nil {
		t.Fatal("Get() should lazy-load even before Start()")
	}
	if string(data) != "hello" {
		t.Errorf("unexpected content: %q", string(data))
	}
	if etag == "" {
		t.Error("etag should not be empty after lazy load")
	}
	t.Logf("lazy loaded before Start(): %d bytes, etag=%s", len(data), etag)

	// Start and verify polling picks up changes since file is now accessed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Start(ctx)

	putObject(t, s3Client, bucket, "watcher-test/noload.txt", "hello-updated")
	time.Sleep(1500 * time.Millisecond)

	data, newEtag := w.Get("noload")
	if string(data) != "hello-updated" {
		t.Errorf("poll should have picked up update, got: %q", data)
	}
	if newEtag == etag {
		t.Error("etag should have changed")
	}

	cancel()
}
