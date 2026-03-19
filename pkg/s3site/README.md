# s3site

The core library for s3site. Discovers tar.gz archives in an S3 bucket,
extracts them (to memory or disk), and routes HTTP requests by Host
header to the matching site.

See the top-level [README](../../README.md) for CLI usage and full docs.

## Library usage

```go
sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:       s3Client,
    Bucket:   "static-assets",
    Prefix:   "sites/",
    Interval: 30 * time.Second,
    Storage:  s3site.StorageMemory, // or s3site.StorageDisk
})

go sm.Start(ctx)

handler := s3site.Handler(sm, logger, "admin.example.com")
http.ListenAndServe(":8080", handler)
```

## Testing

Requires MinIO (via docker-compose in the simples3 repo):

```bash
docker compose up -d
aws --endpoint-url http://127.0.0.1:9000/ s3 mb s3://testbucket

export AWS_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_S3_ACCESS_KEY=minioadmin
export AWS_S3_SECRET_KEY=minioadmin
export AWS_S3_BUCKET=testbucket

go test -v ./...
```

The integration test uploads tar.gz archives to MinIO and verifies:
- Site discovery from bucket listing
- Host-based HTTP routing
- Nested file serving (css/, js/)
- Hot reload on archive update
- Site removal on archive deletion
