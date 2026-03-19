---
title: s3watcher
description: Watch S3 objects for changes with in-memory caching
---

# s3watcher

`pkg/s3watcher` is a standalone package for watching S3 objects. It polls via HEAD requests, keeps file contents in memory, and fires callbacks when objects change.

s3site uses s3watcher internally, but it works fine on its own if you just need to watch a few S3 objects.

## Install

```bash
go get github.com/rhnvrm/s3site/pkg/s3watcher@latest
```

## Basic usage

```go
import "github.com/rhnvrm/s3site/pkg/s3watcher"

w, err := s3watcher.New(s3watcher.Config{
    S3:           s3Client,
    PollInterval: 30 * time.Second,
    Files: []s3watcher.FileEntry{
        {Name: "config", Bucket: "my-bucket", Key: "app/config.json"},
        {Name: "rules",  Bucket: "my-bucket", Key: "app/rules.yaml"},
    },
})

// Register a callback for changes.
w.OnUpdate(func(e s3watcher.UpdateEvent) {
    log.Printf("%s changed (etag: %s -> %s)", e.Name, e.OldETag, e.NewETag)
})

// Start polling in the background.
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
go w.Start(ctx)

// Read a file. Returns content and current ETag.
data, etag := w.Get("config")
```

## Lazy loading

Files are loaded lazily by default. The poll loop only does HEAD requests to check ETags. Content is fetched on the first `Get()` call or `fs.FS` read. If you're watching 50 files but only reading 3, only those 3 get downloaded.

## HEAD-first polling

Each poll cycle:

1. HEAD request to check the ETag
2. If ETag matches the cached version, skip
3. If ETag changed, GET the new content
4. Fire the update callback

This minimizes bandwidth -- most polls are just HEAD requests.

## fs.FS interface

s3watcher exposes watched files as an `fs.FS`:

```go
fsys := w.FS()

// Use with http.FileServerFS, embed, template.ParseFS, etc.
data, err := fs.ReadFile(fsys, "config")
```

File names in the FS are the `Name` fields from your `FileEntry` list.

## Cache management

```go
// Evict a file from memory (re-fetched on next access).
w.Evict("config")

// Get stats.
stats := w.Stats()
fmt.Printf("files: %d, cached: %d, bytes: %d\n",
    stats.Files, stats.Cached, stats.Bytes)
```

## Config

```go
type Config struct {
    S3           *simples3.S3
    PollInterval time.Duration       // default: 1 minute
    Files        []FileEntry
    Logger       *slog.Logger        // optional
}

type FileEntry struct {
    Name   string // lookup key (used in Get, FS, callbacks)
    Bucket string
    Key    string
}
```
