package s3watcher

import (
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

// dummyS3 creates a simples3 client with dummy credentials for unit tests.
func dummyS3() *simples3.S3 {
	return simples3.New("us-east-1", "AKID", "SECRET")
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "nil s3 client",
			cfg:     Config{},
			wantErr: "S3 client is required",
		},
		{
			name:    "no files",
			cfg:     Config{S3: dummyS3()},
			wantErr: "at least one file entry",
		},
		{
			name: "empty name",
			cfg: Config{
				S3:    dummyS3(),
				Files: []FileEntry{{Name: "", Bucket: "b", Key: "k"}},
			},
			wantErr: "name cannot be empty",
		},
		{
			name: "duplicate names",
			cfg: Config{
				S3: dummyS3(),
				Files: []FileEntry{
					{Name: "a", Bucket: "b", Key: "k1"},
					{Name: "a", Bucket: "b", Key: "k2"},
				},
			},
			wantErr: "duplicate file entry name",
		},
		{
			name: "valid config",
			cfg: Config{
				S3:           dummyS3(),
				PollInterval: 5 * time.Second,
				Files: []FileEntry{
					{Name: "instruments", Bucket: "bucket", Key: "instruments.csv"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !containsStr(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetBeforeLoad(t *testing.T) {
	w, err := New(Config{
		S3:    dummyS3(),
		Files: []FileEntry{{Name: "test", Bucket: "b", Key: "k"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, etag := w.Get("test")
	if data != nil {
		t.Errorf("expected nil data before load, got %d bytes", len(data))
	}
	if etag != "" {
		t.Errorf("expected empty etag before load, got %q", etag)
	}

	// Non-existent name.
	data, etag = w.Get("nonexistent")
	if data != nil || etag != "" {
		t.Errorf("expected nil/empty for unknown name")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	w, err := New(Config{
		S3:    dummyS3(),
		Files: []FileEntry{{Name: "test", Bucket: "b", Key: "k"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Manually set state to test copy behavior.
	w.states["test"].data = []byte("hello")
	w.states["test"].etag = "\"abc\""

	data1, _ := w.Get("test")
	data2, _ := w.Get("test")

	// Mutating one copy shouldn't affect the other.
	data1[0] = 'X'
	if string(data2) != "hello" {
		t.Errorf("Get returned shared slice, expected independent copy")
	}
}

func TestOnUpdate(t *testing.T) {
	w, err := New(Config{
		S3:    dummyS3(),
		Files: []FileEntry{{Name: "test", Bucket: "b", Key: "k"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var called int
	w.OnUpdate(func(e UpdateEvent) { called++ })
	w.OnUpdate(func(e UpdateEvent) { called++ })

	if len(w.callbacks) != 2 {
		t.Errorf("expected 2 callbacks, got %d", len(w.callbacks))
	}
}

// --- helpers ---

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
