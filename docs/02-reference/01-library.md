---
title: Go Library
description: Using s3site as a Go package
---

# Go library

The `pkg/s3site` package provides the site manager and HTTP handler for use in your own programs.

## Install

```bash
go get github.com/rhnvrm/s3site@latest
```

## SiteManager

`SiteManager` handles discovery, downloading, extraction, and hot-reloading of sites.

```go
import (
    "github.com/rhnvrm/s3site/pkg/s3site"
    "github.com/rhnvrm/simples3"
)

s3Client := simples3.New("us-east-1", accessKey, secretKey)

sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:       s3Client,
    Bucket:   "my-bucket",
    Prefix:   "sites/",
    Interval: 30 * time.Second,
    Logger:   slog.Default(),
    Storage:  s3site.StorageMemory, // or s3site.StorageDisk
    DataDir:  "/var/lib/s3site",    // only used in disk mode
})

ctx, cancel := context.WithCancel(context.Background())
defer cancel()

go sm.Start(ctx) // blocks until ctx is cancelled
```

### Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `Start(ctx)` | `error` | Run the poll loop. Blocks until context cancellation. |
| `GetSite(hostname)` | `fs.FS` | Get the in-memory filesystem for a hostname. Returns nil if not found. |
| `Hostnames()` | `[]string` | List all loaded hostnames. |
| `S3()` | `*simples3.S3` | The underlying S3 client. |
| `Bucket()` | `string` | The configured bucket. |
| `Prefix()` | `string` | The configured key prefix. |

## Handler

`Handler` returns an `http.Handler` that routes requests by `Host` header to the matching site.

```go
handler := s3site.Handler(sm, logger, "admin.example.com")
http.ListenAndServe(":8080", handler)
```

Parameters:
- `sm` -- the SiteManager
- `logger` -- an `*slog.Logger` (nil for default)
- `adminHost` -- hostname for the admin UI (empty string to disable)

When `adminHost` is set, requests to that host get the admin API. All other hosts are routed to their site's `fs.FS` via `http.FileServerFS`.

## Custom handler

If you need custom routing logic, use `GetSite` directly:

```go
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    host := r.Host
    if i := strings.LastIndex(host, ":"); i != -1 {
        host = host[:i]
    }

    siteFS := sm.GetSite(host)
    if siteFS == nil {
        http.Error(w, "not found", 404)
        return
    }

    // Add your own middleware, logging, etc.
    http.FileServerFS(siteFS).ServeHTTP(w, r)
})
```

## Filesystem

In memory mode, each site is a `memFS` that implements `fs.FS`, `fs.StatFS`, and `fs.ReadFileFS`. Files are byte slices in a flat map; directories are synthesized from paths. In disk mode, sites use `os.DirFS` pointing at the extracted directory.

Both are read-only and safe for concurrent use.
