# s3site

Serve static websites from tar.gz archives in S3. Zero restarts.

Your CI pipeline builds a static site, packs it into a tar.gz named
after the hostname, and pushes it to S3. s3site picks it up and serves
it. When the archive changes, the site hot-reloads. Delete the archive
and the site goes away.

```
s3://static-assets/sites/
  foo.example.com.tar.gz     -> serves foo.example.com
  bar.example.com.tar.gz     -> serves bar.example.com
  docs.mysite.org.tar.gz     -> serves docs.mysite.org
```

## Install

```bash
go install github.com/rhnvrm/s3site/cmd/s3site@latest
```

## Usage

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -region us-east-1 \
  -listen :8080 \
  -poll 30s
```

For MinIO or S3-compatible stores:

```bash
s3site -bucket my-bucket -endpoint http://minio:9000 -listen :8080
```

### Admin UI

Set `-admin-host` to serve an admin UI on a dedicated hostname:

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -listen :8080 \
  -admin-host admin.example.com
```

The admin UI at `admin.example.com` lets you list, upload, and delete
sites from the browser. No admin endpoints are exposed on hosted sites.

### Flags and environment variables

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-bucket` | `AWS_S3_BUCKET` | (required) | S3 bucket name |
| `-region` | `AWS_S3_REGION` | `us-east-1` | AWS region |
| `-access-key` | `AWS_S3_ACCESS_KEY` | | AWS access key |
| `-secret-key` | `AWS_S3_SECRET_KEY` | | AWS secret key |
| `-endpoint` | `AWS_S3_ENDPOINT` | | Custom S3 endpoint |
| `-prefix` | `S3SITE_PREFIX` | | S3 key prefix |
| `-listen` | `S3SITE_LISTEN` | `:8080` | HTTP listen address |
| `-poll` | `S3SITE_POLL` | `30s` | Poll interval |
| `-admin-host` | `S3SITE_ADMIN_HOST` | | Hostname for admin UI |
| `-storage` | `S3SITE_STORAGE` | `memory` | Storage mode: `memory` or `disk` |
| `-data-dir` | `S3SITE_DATA_DIR` | `$TMPDIR/s3site-data` | Directory for disk storage |

## CI/CD deployment

```yaml
deploy:
  script:
    - hugo  # or jekyll, mkdocs, npm run build, etc.
    - tar czf site.tar.gz -C public .
    - aws s3 cp site.tar.gz s3://static-assets/sites/docs.example.com.tar.gz
```

The archive filename (minus `.tar.gz`) becomes the hostname. Files
should be at the root of the archive (not wrapped in a directory).

## Packages

### `pkg/s3watcher`

An in-memory file manager that watches S3 objects for changes. Polls
HEAD/ETag, keeps contents in memory, fires callbacks on change. Supports
lazy loading and exposes an `fs.FS` interface.

```go
w, _ := s3watcher.New(s3watcher.Config{
    S3:           s3Client,
    PollInterval: 30 * time.Second,
    Files: []s3watcher.FileEntry{
        {Name: "instruments", Bucket: "b", Key: "data/instruments.csv"},
    },
})

w.OnUpdate(func(e s3watcher.UpdateEvent) {
    log.Printf("%s changed", e.Name)
})

go w.Start(ctx)
data, etag := w.Get("instruments")
```

See [pkg/s3watcher/README.md](pkg/s3watcher/README.md) for full docs.

### `pkg/s3site`

The static site serving library. Discovers tar.gz archives in a bucket,
extracts them into in-memory filesystems, and routes HTTP requests by
Host header.

```go
sm := s3site.NewSiteManager(s3site.SiteManagerConfig{
    S3:       s3Client,
    Bucket:   "static-assets",
    Prefix:   "sites/",
    Interval: 30 * time.Second,
})

go sm.Start(ctx)

handler := s3site.Handler(sm, logger, "admin.example.com")
http.ListenAndServe(":8080", handler)
```

Admin API (on the admin host only):
- `GET /api/sites` - JSON array of hostnames
- `POST /api/upload` - multipart form: `hostname` + `file` (tar.gz)
- `POST /api/delete` - JSON body: `{"hostname": "..."}`
- `GET /health` - returns `ok`

See [pkg/s3site/README.md](pkg/s3site/README.md) for full docs.

## How it works

1. Lists the S3 bucket for `*.tar.gz` files under the configured prefix
2. Downloads each archive, extracts into an in-memory `fs.FS`
3. Routes HTTP requests by `Host` header to the matching site
4. Polls for changes - compares ETags, re-downloads on change
5. Removes sites whose archives are deleted

In memory mode (default), all files are served from RAM. In disk mode
(`-storage disk`), sites are extracted to a local directory and served
from disk, keeping memory usage flat regardless of site count.

## Development

Requires Docker for MinIO:

```bash
# Start MinIO (from the simples3 repo or any docker-compose with MinIO)
docker compose up -d
aws --endpoint-url http://127.0.0.1:9000/ s3 mb s3://testbucket

# Run all tests
export AWS_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_S3_ACCESS_KEY=minioadmin
export AWS_S3_SECRET_KEY=minioadmin
export AWS_S3_BUCKET=testbucket
go test ./...
```

## License

BSD-2-Clause
