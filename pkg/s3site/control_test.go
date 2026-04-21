package s3site

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveExistingSocketRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.WriteFile(path, []byte("not-a-socket"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := removeExistingSocket(path); err == nil {
		t.Fatal("expected regular file to be rejected")
	}
}

func TestRemoveExistingSocketRemovesUnixSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	if err := removeExistingSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected socket path to be removed, got %v", err)
	}
}
