# s3watcher

An in-memory file manager that watches S3 objects for changes. It polls
HEAD/ETag at a configurable interval, keeps file contents in memory, and
fires callbacks when files change. Built on top of
[simples3](https://github.com/rhnvrm/simples3).

## Problem

You have config files or data files on S3 (instruments lists, holiday
calendars, feature flags) that your app needs at runtime. Currently you
either restart the app or run a sidecar to sync files. s3watcher does
this in-process with no restarts.

## Install

```
go get github.com/rhnvrm/s3site/pkg/s3watcher
```

## Usage

```go
s3Client := simples3.New("us-east-1", accessKey, secretKey)

w, err := s3watcher.New(s3watcher.Config{
    S3:           s3Client,
    PollInterval: 30 * time.Second,
    Files: []s3watcher.FileEntry{
        {Name: "instruments", Bucket: "my-bucket", Key: "data/instruments.csv"},
        {Name: "holidays",    Bucket: "my-bucket", Key: "config/holidays.json"},
    },
})

// Register callbacks for changes.
w.OnUpdate(func(e s3watcher.UpdateEvent) {
    log.Printf("%s changed (%d bytes)", e.Name, len(e.Data))
})

// Start polling in the background.
go w.Start(ctx)

// Read files anytime. First call lazy-loads from S3.
data, etag := w.Get("instruments")
```

## How it works

1. **Lazy loading** - files are not fetched until the first `Get()` or
   `FS` read. No wasted bandwidth on files your code never reads.

2. **HEAD-first polling** - each poll cycle sends a HEAD request per
   watched file. Only if the ETag differs from the cached version does
   it download the body. Unaccessed files are skipped entirely.

3. **Callbacks** - when a file changes, all registered `OnUpdate`
   functions are called synchronously with the new data. Keep callbacks
   fast or spawn a goroutine for heavy work.

4. **Thread-safe reads** - `Get()` returns a copy of the data. Multiple
   goroutines can read concurrently without coordination.

## API

### Config

```go
s3watcher.Config{
    S3           *simples3.S3    // Required. The simples3 client.
    PollInterval time.Duration   // How often to poll. Default: 1 minute.
    Files        []FileEntry     // S3 objects to watch.
    Logger       *slog.Logger    // Optional. Default: slog.Default().
    LoadOnStart  *bool           // Fetch all files eagerly on Start(). Default: true.
}
```

When `LoadOnStart` is true (default), `Start()` downloads all files
immediately and the poll loop watches them all. When false, files are
purely lazy -- fetched on first access and polled only after that.

### Reading files

```go
// Get returns a copy of the cached bytes and the current ETag.
// Lazy-loads from S3 on first call.
data, etag := w.Get("instruments")
```

### Callbacks

```go
w.OnUpdate(func(e s3watcher.UpdateEvent) {
    // e.Name         - logical file name
    // e.Data         - new file contents
    // e.ETag         - new ETag
    // e.LastModified - Last-Modified header value
})
```

Multiple callbacks can be registered. They fire on both initial load and
subsequent changes.

### Cache management

```go
// Evict one file. Next Get() lazy-loads it fresh from S3.
// Poll loop stops watching it until re-accessed.
w.Clear("instruments")

// Evict all files.
w.ClearAll()

// Force immediate re-fetch without waiting for the next poll tick.
err := w.Refresh("instruments")
```

### fs.FS interface

The watcher exposes an `fs.FS` backed by its in-memory cache. This is a
live view -- reads always return the latest data. It implements `fs.FS`,
`fs.StatFS`, and `fs.ReadFileFS`, and passes `testing/fstest.TestFS`.

```go
fsys := w.FS()

// Works with the standard library.
data, err := fs.ReadFile(fsys, "instruments")
entries, err := fs.ReadDir(fsys, ".")
info, err := fs.Stat(fsys, "instruments")

// Drop into anything that takes fs.FS.
http.FileServer(http.FS(fsys))
tmpl.ParseFS(fsys, "*.tmpl")
```

File names are the logical `Name` from `FileEntry`, not S3 paths. The
namespace is flat (no subdirectories). Unloaded files return
`fs.ErrNotExist` and trigger a lazy load on access.

## Testing

Tests run against MinIO via docker-compose:

```bash
# Start MinIO
docker compose up -d
sleep 3
aws --endpoint-url http://127.0.0.1:9000/ s3 mb s3://testbucket

# Run tests
export AWS_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_S3_REGION=us-east-1
export AWS_S3_ACCESS_KEY=minioadmin
export AWS_S3_SECRET_KEY=minioadmin
export AWS_S3_BUCKET=testbucket

go test -v ./...

# Or use the Justfile from the parent repo
just setup
go test -v ./...
```

Unit tests (validation, copy safety, FS compliance) run without MinIO.
Integration tests cover the full lifecycle: initial load, change
detection, lazy loading, cache eviction, and refresh.

Use `-short` to skip integration tests:

```bash
go test -short ./...
```

## Design notes

- **No interfaces to mock** - the package uses `*simples3.S3` directly.
  For testing, point it at MinIO.
- **Flat namespace** - file names are logical identifiers, not paths.
  This keeps the FS implementation simple and avoids path separator
  issues across platforms.
- **Synchronous callbacks** - called inside the poll goroutine. This is
  intentional: it ensures callbacks see a consistent snapshot and avoids
  ordering issues. Spawn your own goroutine if the callback does heavy
  work.
- **No file-level polling config** - all files share the same poll
  interval. If you need different intervals, create multiple watchers.
- **`ModTime` returns zero** - the FS does not track modification times.
  ETags are the source of truth for change detection.
