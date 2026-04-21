# s3site

The core library for s3site. It can run in:

- **discovery mode**: list `*.tar.gz` archives from a bucket prefix
- **hosted mode**: serve only explicitly declared sites from stable object keys

See the top-level [README](../../README.md) for CLI usage and deployment guidance.

## Library usage

### Discovery mode

```go
sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:       s3Client,
    Bucket:   "static-assets",
    Prefix:   "sites/",
    Interval: 30 * time.Second,
    Storage:  s3site.StorageMemory,
})

if err := sm.Validate(); err != nil {
    panic(err)
}

go sm.Start(ctx)

handler := s3site.Handler(sm, logger, "")
http.ListenAndServe(":8080", handler)
```

### Hosted mode

Load declared sites from JSON, then run with a local control socket:

```go
sites, err := s3site.LoadHostedSites("/etc/s3site/sites.json", "sites/")
if err != nil {
    panic(err)
}

sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:          s3Client,
    Bucket:      "static-assets",
    Prefix:      "sites/",
    Interval:    10 * time.Minute,
    Storage:     s3site.StorageDisk,
    HostedSites: sites,
})
if err := sm.Validate(); err != nil {
    panic(err)
}

go sm.Start(ctx)
go s3site.StartControlServer(ctx, "/run/s3site/control.sock", sm, logger)
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

When `key` is omitted, `LoadHostedSites` derives it from `prefix + hostname + ".tar.gz"`.

## Notes

- `HostedSites` disables bucket-wide discovery and serves declared sites only.
- `RefreshHosts()` is intended for local control-plane use.
- `StartControlServer()` exposes `/refresh` and `/health` on a unix socket.
- Disk mode uses versioned directories and retains the previous generation for one swap.
- Archive extraction rejects path traversal and enforces file-count and total-size limits.

## Testing

Requires MinIO for full integration coverage:

```bash
export AWS_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_S3_ACCESS_KEY=minioadmin
export AWS_S3_SECRET_KEY=minioadmin
export AWS_S3_BUCKET=testbucket

go test -short ./...
go test ./...
```
