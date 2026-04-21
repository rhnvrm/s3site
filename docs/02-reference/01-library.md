---
title: Go Library
description: Using s3site as a Go package
---

# Go library

The `pkg/s3site` package provides the site manager, HTTP handler, hosted-site config loader, and local control server.

## Install

```bash
go get github.com/rhnvrm/s3site@latest
```

## SiteManager

`SiteManager` handles discovery, downloading, extraction, and hot-reloading of sites.

### Discovery mode

```go
sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:       s3Client,
    Bucket:   "my-bucket",
    Prefix:   "sites/",
    Interval: 30 * time.Second,
    Logger:   slog.Default(),
    Storage:  s3site.StorageMemory,
})
if err := sm.Validate(); err != nil {
    panic(err)
}

go sm.Start(ctx)
```

### Hosted mode

```go
sites, err := s3site.LoadHostedSites("/etc/s3site/sites.json", "sites/")
if err != nil {
    panic(err)
}

sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:          s3Client,
    Bucket:      "my-bucket",
    Prefix:      "sites/",
    Interval:    10 * time.Minute,
    Logger:      slog.Default(),
    Storage:     s3site.StorageDisk,
    DataDir:     "/var/lib/s3site",
    HostedSites: sites,
})
if err := sm.Validate(); err != nil {
    panic(err)
}

go sm.Start(ctx)
go s3site.StartControlServer(ctx, "/run/s3site/control.sock", sm, slog.Default())
```

Hosted config format:

```json
{
  "sites": [
    { "hostname": "rohanverma.net" },
    { "hostname": "oddship.net", "key": "sites/oddship.net.tar.gz" }
  ]
}
```

If `key` is omitted, it defaults to `prefix + hostname + ".tar.gz"`.

### Methods

| Method | Returns | Description |
|--------|---------|-------------|
| `Validate()` | `error` | Validate manager configuration before starting. |
| `Start(ctx)` | `error` | Run the sync loop. Blocks until context cancellation. |
| `Ready()` | `bool` | Reports whether the initial sync attempt has completed. |
| `HostedMode()` | `bool` | Reports whether explicit hosted sites are configured. |
| `RefreshHosts(hosts)` | `error` | Force refresh for declared hosted sites. |
| `GetSite(hostname)` | `fs.FS` | Get the filesystem for a hostname. Returns nil if not found. |
| `Hostnames()` | `[]string` | List all loaded hostnames. |
| `S3()` | `*simples3.S3` | The underlying S3 client. |
| `Bucket()` | `string` | The configured bucket. |
| `Prefix()` | `string` | The configured key prefix. |

## Handler

`Handler` returns an `http.Handler` that routes requests by normalized `Host` header to the matching site.

```go
handler := s3site.Handler(sm, logger, "")
http.ListenAndServe(":8080", handler)
```

Parameters:
- `sm` -- the SiteManager
- `logger` -- an `*slog.Logger` (nil for default)
- `adminHost` -- hostname for the legacy browser admin UI (empty string to disable)

When `adminHost` is set, requests to that host get the admin API. All other hosts are routed to their site's `fs.FS` via `http.FileServerFS`.

## Local control server

`StartControlServer` exposes a local-only control plane over a unix socket.

Endpoints:
- `GET /health`
- `POST /refresh`

```go
go s3site.StartControlServer(ctx, "/run/s3site/control.sock", sm, logger)
```

## Custom handler

If you need custom routing logic, use `GetSite` directly:

```go
http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    siteFS := sm.GetSite(r.Host)
    if siteFS == nil {
        http.Error(w, "not found", 404)
        return
    }

    http.FileServerFS(siteFS).ServeHTTP(w, r)
})
```

## Filesystem and extraction safety

In memory mode, each site is a `memFS` that implements `fs.FS`, `fs.StatFS`, and `fs.ReadFileFS`.

In disk mode, sites use `os.DirFS` backed by versioned extraction directories. The previous on-disk version is retained for one activation to reduce swap races.

Extraction rejects path traversal and enforces archive file-count and total-size limits.
