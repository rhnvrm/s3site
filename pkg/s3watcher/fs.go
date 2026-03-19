package s3watcher

import (
	"io"
	"io/fs"
	"strings"
	"time"
)

// Compile-time interface checks.
var (
	_ fs.FS         = (*FS)(nil)
	_ fs.StatFS     = (*FS)(nil)
	_ fs.ReadFileFS = (*FS)(nil)
	_ fs.File       = (*memFile)(nil)
	_ fs.FileInfo   = (*memFileInfo)(nil)
)

// FS returns an fs.FS backed by the watcher's in-memory file store.
// Files are accessible by their logical name (the Name field in FileEntry).
//
// The returned FS is a live view - reads always return the latest data
// the watcher has fetched. Thread-safe.
func (w *Watcher) FS() *FS {
	return &FS{w: w}
}

// FS implements fs.FS, fs.StatFS, and fs.ReadFileFS over the watcher's
// in-memory files. File names are the logical names from FileEntry.
type FS struct {
	w *Watcher
}

// Open opens the named file. The name is the logical file name (FileEntry.Name).
// It also supports "." to open the root directory.
func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	// Root directory.
	if name == "." {
		return f.openDir()
	}

	data, etag, err := f.readFile(name)
	if err != nil {
		return nil, err
	}

	return &memFile{
		info:   newMemFileInfo(name, data),
		reader: strings.NewReader(string(data)),
		etag:   etag,
	}, nil
}

// ReadFile implements fs.ReadFileFS for efficient reads without Open/Read/Close.
func (f *FS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}

	data, _, err := f.readFile(name)
	return data, err
}

// Stat implements fs.StatFS.
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}

	if name == "." {
		return &memFileInfo{name: ".", isDir: true}, nil
	}

	data, _, err := f.readFile(name)
	if err != nil {
		return nil, err
	}

	return newMemFileInfo(name, data), nil
}

// readFile looks up a file by name, triggers a lazy load if needed,
// and returns a copy of its data.
func (f *FS) readFile(name string) ([]byte, string, error) {
	st, ok := f.w.states[name]
	if !ok {
		return nil, "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	// Trigger lazy load if this is the first access.
	f.w.ensureLoaded(name, st)

	st.mu.RLock()
	defer st.mu.RUnlock()

	if st.data == nil {
		// Lazy load was attempted but failed (e.g. network error).
		return nil, "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	cp := make([]byte, len(st.data))
	copy(cp, st.data)
	return cp, st.etag, nil
}

// openDir returns a directory listing of all loaded files.
func (f *FS) openDir() (fs.File, error) {
	var entries []fs.DirEntry
	for _, fe := range f.w.files {
		st := f.w.states[fe.Name]
		st.mu.RLock()
		if st.data != nil {
			entries = append(entries, &memDirEntry{info: newMemFileInfo(fe.Name, st.data)})
		}
		st.mu.RUnlock()
	}

	return &memDir{
		info:    &memFileInfo{name: ".", isDir: true},
		entries: entries,
	}, nil
}

// --- memFile ---

// memFile is an in-memory fs.File.
type memFile struct {
	info   *memFileInfo
	reader *strings.Reader
	etag   string
}

func (f *memFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memFile) Close() error                { return nil }

func (f *memFile) Read(b []byte) (int, error) {
	return f.reader.Read(b)
}

// --- memFileInfo ---

type memFileInfo struct {
	name  string
	size  int64
	isDir bool
}

func newMemFileInfo(name string, data []byte) *memFileInfo {
	return &memFileInfo{name: name, size: int64(len(data))}
}

func (fi *memFileInfo) Name() string      { return fi.name }
func (fi *memFileInfo) Size() int64       { return fi.size }
func (fi *memFileInfo) Mode() fs.FileMode { return 0444 }
func (fi *memFileInfo) ModTime() time.Time { return time.Time{} }
func (fi *memFileInfo) IsDir() bool       { return fi.isDir }
func (fi *memFileInfo) Sys() any          { return nil }

// --- memDir ---

// memDir is an in-memory directory that implements fs.ReadDirFile.
type memDir struct {
	info    *memFileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *memDir) Stat() (fs.FileInfo, error)        { return d.info, nil }
func (d *memDir) Close() error                       { return nil }
func (d *memDir) Read([]byte) (int, error)           { return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid} }

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

func (e *memDirEntry) Name() string               { return e.info.Name() }
func (e *memDirEntry) IsDir() bool                { return false }
func (e *memDirEntry) Type() fs.FileMode           { return 0 }
func (e *memDirEntry) Info() (fs.FileInfo, error)  { return e.info, nil }
