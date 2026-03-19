package s3site

import (
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// Compile-time checks.
var (
	_ fs.FS         = (*memFS)(nil)
	_ fs.StatFS     = (*memFS)(nil)
	_ fs.ReadFileFS = (*memFS)(nil)
)

// memFS is an in-memory filesystem extracted from a tar.gz archive.
// It supports nested directories for serving static sites.
type memFS struct {
	files map[string][]byte // path -> content
}

func newMemFS() *memFS {
	return &memFS{files: make(map[string][]byte)}
}

// Add adds a file to the FS.
func (m *memFS) Add(name string, data []byte) {
	m.files[name] = data
}

// Len returns the number of files.
func (m *memFS) Len() int {
	return len(m.files)
}

// Open opens the named file or directory.
func (m *memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	// Check if it's a file.
	if data, ok := m.files[name]; ok {
		return &memFile{
			info:   &memFileInfo{name: path.Base(name), size: int64(len(data))},
			reader: strings.NewReader(string(data)),
		}, nil
	}

	// Check if it's a directory (has files under it).
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}

	var entries []fs.DirEntry
	seen := make(map[string]bool) // track immediate children

	for fpath, data := range m.files {
		var rel string
		if prefix == "" {
			rel = fpath
		} else if strings.HasPrefix(fpath, prefix) {
			rel = strings.TrimPrefix(fpath, prefix)
		} else {
			continue
		}

		// Get the immediate child name.
		parts := strings.SplitN(rel, "/", 2)
		child := parts[0]
		if child == "" || seen[child] {
			continue
		}
		seen[child] = true

		if len(parts) > 1 {
			// It's a subdirectory.
			entries = append(entries, &memDirEntry{
				info: &memFileInfo{name: child, isDir: true},
			})
		} else {
			// It's a file.
			entries = append(entries, &memDirEntry{
				info: &memFileInfo{name: child, size: int64(len(data))},
			})
		}
	}

	if len(entries) == 0 && name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	dirName := path.Base(name)
	if name == "." {
		dirName = "."
	}

	return &memDir{
		info:    &memFileInfo{name: dirName, isDir: true},
		entries: entries,
	}, nil
}

// ReadFile implements fs.ReadFileFS.
func (m *memFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}

	data, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}

	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

// Stat implements fs.StatFS.
func (m *memFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}

	if name == "." {
		return &memFileInfo{name: ".", isDir: true}, nil
	}

	if data, ok := m.files[name]; ok {
		return &memFileInfo{name: path.Base(name), size: int64(len(data))}, nil
	}

	// Check if it's a directory.
	prefix := name + "/"
	for fpath := range m.files {
		if strings.HasPrefix(fpath, prefix) {
			return &memFileInfo{name: path.Base(name), isDir: true}, nil
		}
	}

	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

// --- memFile ---

type memFile struct {
	info   *memFileInfo
	reader *strings.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error                { return nil }
func (f *memFile) Read(b []byte) (int, error)  { return f.reader.Read(b) }

// --- memFileInfo ---

type memFileInfo struct {
	name  string
	size  int64
	isDir bool
}

func (fi *memFileInfo) Name() string        { return fi.name }
func (fi *memFileInfo) Size() int64         { return fi.size }
func (fi *memFileInfo) Mode() fs.FileMode {
	if fi.isDir {
		return fs.ModeDir | 0555
	}
	return 0444
}
func (fi *memFileInfo) ModTime() time.Time  { return time.Time{} }
func (fi *memFileInfo) IsDir() bool         { return fi.isDir }
func (fi *memFileInfo) Sys() any            { return nil }

// --- memDir ---

type memDir struct {
	info    *memFileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *memDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memDir) Close() error               { return nil }
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}

func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		entries := d.entries[d.offset:]
		d.offset = len(d.entries)
		return entries, nil
	}

	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}

	end := d.offset + n
	if end > len(d.entries) {
		end = len(d.entries)
	}

	entries := d.entries[d.offset:end]
	d.offset = end

	if d.offset >= len(d.entries) {
		return entries, io.EOF
	}
	return entries, nil
}

// --- memDirEntry ---

type memDirEntry struct {
	info *memFileInfo
}

func (e *memDirEntry) Name() string              { return e.info.Name() }
func (e *memDirEntry) IsDir() bool               { return e.info.IsDir() }
func (e *memDirEntry) Type() fs.FileMode         { return e.info.Mode().Type() }
func (e *memDirEntry) Info() (fs.FileInfo, error) { return e.info, nil }
