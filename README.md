# s3site

Serve static websites from tar.gz archives in S3-compatible object storage with hot reloads.

`s3site` now supports two operating modes:

1. **Discovery mode** — legacy bucket scan for `*.tar.gz` objects under a prefix.
2. **Hosted mode** — explicitly declared sites backed by stable object keys, with local-only refresh control.

Hosted mode is the recommended path for running `s3site` behind a declarative ingress layer such as Caddy/Nix.

## Install

```bash
go install github.com/rhnvrm/s3site/cmd/s3site@latest
```

## Quick start

### Discovery mode

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -region us-east-1 \
  -listen :8080 \
  -poll 30s
```

Objects under the prefix are mapped by filename:

```txt
s3://static-assets/sites/
  foo.example.com.tar.gz     -> serves foo.example.com
  bar.example.com.tar.gz     -> serves bar.example.com
```

### Hosted mode

Declare the sites that are allowed to go live:

```json
{
  "sites": [
    { "hostname": "rohanverma.net" },
    { "hostname": "oddship.net" }
  ]
}
```

Start `s3site` with a local control socket:

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -sites-config /etc/s3site/sites.json \
  -control-socket /run/s3site/control.sock \
  -listen 127.0.0.1:9001 \
  -poll 10m
```

When `key` is omitted, `s3site` derives it as:

```txt
<prefix><hostname>.tar.gz
```

So the config above watches:

```txt
sites/rohanverma.net.tar.gz
sites/oddship.net.tar.gz
```

Refresh one or more declared sites after CI uploads a new tarball:

```bash
s3site refresh -socket /run/s3site/control.sock rohanverma.net
```

That keeps deploys fast without requiring frequent bucket scans.

## Production model

For hosted deployments, the intended split is:

- **Caddy / Nix / ingress layer**: TLS, public domains, reverse proxying, service wiring
- **s3site**: content activation, object fetch, hot reload
- **CI**: build tarball, upload to a stable key, call local refresh

## Flags and environment variables

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
| `-sites-config` | `S3SITE_SITES_CONFIG` | | Hosted-sites JSON config |
| `-control-socket` | `S3SITE_CONTROL_SOCKET` | | Unix socket for local refresh control |
| `-admin-host` | `S3SITE_ADMIN_HOST` | | Hostname for the insecure dev admin UI |
| `-allow-insecure-admin` | `S3SITE_ALLOW_INSECURE_ADMIN` | `false` | Explicitly enable the insecure public admin UI/API |
| `-storage` | `S3SITE_STORAGE` | `memory` | Storage mode: `memory` or `disk` |
| `-data-dir` | `S3SITE_DATA_DIR` | `$TMPDIR/s3site-data` | Directory for disk storage |

## Refresh flow

Typical CI flow in hosted mode:

```yaml
deploy:
  script:
    - hugo
    - tar czf site.tar.gz -C public .
    - aws s3 cp site.tar.gz s3://static-assets/sites/rohanverma.net.tar.gz
    - ssh oddship-web 's3site refresh -socket /run/s3site/control.sock rohanverma.net'
```

Rollback is simply rerunning CI from an older commit and uploading to the same key.

## Admin UI

The browser admin UI still exists for development, but it is intentionally gated behind `-allow-insecure-admin` because it has no authentication.

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -admin-host admin.example.com \
  -allow-insecure-admin
```

Do **not** expose that mode on the public internet unless you add your own auth and network isolation.

## How it works

### Discovery mode

1. List `*.tar.gz` objects under the configured prefix
2. Map archive filename to hostname
3. Compare ETags against current state
4. Download and extract only changed archives
5. Remove sites whose archives were deleted

### Hosted mode

1. Load a declared site registry from JSON
2. Watch declared object keys only
3. Compare ETags for declared keys only
4. Download and activate updated archives
5. Keep the previous on-disk version around for one generation in disk mode to reduce swap races

## Storage modes

In memory mode (default), all files are served from RAM.

In disk mode (`-storage disk`), extracted sites are served from versioned directories on disk. `s3site` retains the previous on-disk version for one generation after an activation so a swap does not immediately delete the just-replaced tree.

| Mode | Pros | Cons |
|------|------|------|
| `memory` | Fast serving, no disk I/O | Uses RAM for all site content |
| `disk` | Minimal RAM usage | Disk I/O on every request, needs writable directory |

## Safety improvements in hosted mode

- only declared sites may go live
- hostname validation is strict
- tar path traversal is rejected
- archive file count and total extracted bytes are bounded
- refresh control is local-only via unix socket
- server timeouts and `SIGTERM` handling are enabled

## Development

Requires Docker for MinIO:

```bash
# Start MinIO
export AWS_S3_ENDPOINT=http://127.0.0.1:9000
export AWS_S3_ACCESS_KEY=minioadmin
export AWS_S3_SECRET_KEY=minioadmin
export AWS_S3_BUCKET=testbucket

go test ./... -short
```

Run full integration tests with a live MinIO instance on `127.0.0.1:9000`.

## Packages

### `pkg/s3site`

Static site serving library for both discovery mode and hosted mode.

### `pkg/s3watcher`

In-memory file watcher for arbitrary S3 objects. See [pkg/s3watcher/README.md](pkg/s3watcher/README.md).

## License

BSD-2-Clause
