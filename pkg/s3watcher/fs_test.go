package s3watcher

import (
	"context"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rhnvrm/simples3"
)

func TestFS_Unit(t *testing.T) {
	w, err := New(Config{
		S3:    simples3.New("us-east-1", "x", "x"),
		Files: []FileEntry{{Name: "a.txt", Bucket: "b", Key: "k"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	fsys := w.FS()

	// File not loaded yet - should return ErrNotExist.
	_, err = fsys.Open("a.txt")
	if err == nil {
		t.Fatal("expected error for unloaded file")
	}
	if !isNotExist(err) {
		t.Fatalf("expected ErrNotExist, got: %v", err)
	}

	// Non-existent file.
	_, err = fsys.Open("nope.txt")
	if !isNotExist(err) {
		t.Fatalf("expected ErrNotExist for unknown file, got: %v", err)
	}

	// Invalid path.
	_, err = fsys.Open("/absolute")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}

	// Populate state and re-test.
	w.states["a.txt"].mu.Lock()
	w.states["a.txt"].data = []byte("contents")
	w.states["a.txt"].etag = `"abc"`
	w.states["a.txt"].mu.Unlock()

	// Open and read.
	f, err := fsys.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "contents" {
		t.Errorf("got %q, want %q", data, "contents")
	}

	// Stat.
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "a.txt" {
		t.Errorf("name = %q, want %q", info.Name(), "a.txt")
	}
	if info.Size() != 8 {
		t.Errorf("size = %d, want 8", info.Size())
	}
	if info.IsDir() {
		t.Error("should not be a directory")
	}

	// ReadFile.
	data, err = fsys.ReadFile("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "contents" {
		t.Errorf("ReadFile got %q, want %q", data, "contents")
	}

	// Stat via StatFS.
	info, err = fsys.Stat("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 8 {
		t.Errorf("Stat size = %d, want 8", info.Size())
	}

	// Root directory.
	info, err = fsys.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Error(". should be a directory")
	}
}

func TestFS_ReadDir(t *testing.T) {
	w, err := New(Config{
		S3: simples3.New("us-east-1", "x", "x"),
		Files: []FileEntry{
			{Name: "a", Bucket: "b", Key: "k1"},
			{Name: "b", Bucket: "b", Key: "k2"},
			{Name: "c", Bucket: "b", Key: "k3"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Load a and c but not b.
	w.states["a"].data = []byte("aa")
	w.states["a"].etag = `"1"`
	w.states["c"].data = []byte("cc")
	w.states["c"].etag = `"3"`

	fsys := w.FS()
	f, err := fsys.Open(".")
	if err != nil {
		t.Fatal(err)
	}

	dir, ok := f.(fs.ReadDirFile)
	if !ok {
		t.Fatal("root should implement ReadDirFile")
	}

	entries, err := dir.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}

	// Only loaded files should appear.
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Name() != "a" || entries[1].Name() != "c" {
		t.Errorf("entries = [%s, %s], want [a, c]", entries[0].Name(), entries[1].Name())
	}
}

func TestFS_LiveView(t *testing.T) {
	// Verify that FS reflects updates to the watcher's state.
	w, err := New(Config{
		S3:    simples3.New("us-east-1", "x", "x"),
		Files: []FileEntry{{Name: "live", Bucket: "b", Key: "k"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	fsys := w.FS()

	// Initially not loaded.
	_, err = fsys.ReadFile("live")
	if !isNotExist(err) {
		t.Fatalf("expected not exist, got: %v", err)
	}

	// Simulate watcher updating the file.
	w.states["live"].mu.Lock()
	w.states["live"].data = []byte("v1")
	w.states["live"].etag = `"v1"`
	w.states["live"].mu.Unlock()

	data, err := fsys.ReadFile("live")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1" {
		t.Errorf("got %q, want v1", data)
	}

	// Update again.
	w.states["live"].mu.Lock()
	w.states["live"].data = []byte("v2-updated")
	w.states["live"].etag = `"v2"`
	w.states["live"].mu.Unlock()

	data, err = fsys.ReadFile("live")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2-updated" {
		t.Errorf("got %q, want v2-updated", data)
	}
}

func TestFS_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	s3Client, bucket := setupIntegrationS3(t)

	putObject(t, s3Client, bucket, "watcher-test/fs-a.txt", "file-a-content")
	putObject(t, s3Client, bucket, "watcher-test/fs-b.txt", "file-b-content")
	t.Cleanup(func() {
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/fs-a.txt"})
		s3Client.FileDelete(simples3.DeleteInput{Bucket: bucket, ObjectKey: "watcher-test/fs-b.txt"})
	})

	loadOnStart := true
	w, err := New(Config{
		S3:           s3Client,
		PollInterval: 500 * time.Millisecond,
		Files: []FileEntry{
			{Name: "fs-a.txt", Bucket: bucket, Key: "watcher-test/fs-a.txt"},
			{Name: "fs-b.txt", Bucket: bucket, Key: "watcher-test/fs-b.txt"},
		},
		LoadOnStart: &loadOnStart,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Start just to trigger initial load (LoadOnStart), then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	time.Sleep(500 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)

	fsys := w.FS()

	// Use Go's fstest.TestFS to validate the FS implementation.
	if err := fstest.TestFS(fsys, "fs-a.txt", "fs-b.txt"); err != nil {
		t.Fatal(err)
	}

	// Verify content.
	data, err := fs.ReadFile(fsys, "fs-a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "file-a-content" {
		t.Errorf("got %q, want %q", data, "file-a-content")
	}
}

func isNotExist(err error) bool {
	pe, ok := err.(*fs.PathError)
	if ok {
		return pe.Err == fs.ErrNotExist
	}
	return false
}
