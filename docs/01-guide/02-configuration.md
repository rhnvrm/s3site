---
title: Configuration
description: CLI flags and environment variables
---

# Configuration

s3site is configured via CLI flags. Every flag has a corresponding environment variable.

## Flags

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `-bucket` | `AWS_S3_BUCKET` | (required) | S3 bucket name |
| `-region` | `AWS_S3_REGION` | `us-east-1` | AWS region |
| `-access-key` | `AWS_S3_ACCESS_KEY` | | AWS access key |
| `-secret-key` | `AWS_S3_SECRET_KEY` | | AWS secret key |
| `-endpoint` | `AWS_S3_ENDPOINT` | | Custom S3 endpoint (MinIO, etc.) |
| `-prefix` | `S3SITE_PREFIX` | | S3 key prefix for archives |
| `-listen` | `S3SITE_LISTEN` | `:8080` | HTTP listen address |
| `-poll` | `S3SITE_POLL` | `30s` | Poll interval for changes |
| `-sites-config` | `S3SITE_SITES_CONFIG` | | Hosted-sites JSON config |
| `-control-socket` | `S3SITE_CONTROL_SOCKET` | | Unix socket for local refresh control |
| `-admin-host` | `S3SITE_ADMIN_HOST` | | Hostname for the insecure dev admin UI |
| `-allow-insecure-admin` | `S3SITE_ALLOW_INSECURE_ADMIN` | `false` | Explicitly enable the insecure admin UI/API |
| `-storage` | `S3SITE_STORAGE` | `memory` | Storage mode: `memory` or `disk` |
| `-data-dir` | `S3SITE_DATA_DIR` | `$TMPDIR/s3site-data` | Directory for disk storage |

Environment variables take effect only when the flag is not set. Flags always win.

## Discovery mode

Without `-sites-config`, s3site runs in discovery mode.

The prefix scopes s3site to a subdirectory in the bucket:

```txt
s3://shared-bucket/
  sites/
    foo.example.com.tar.gz
    bar.example.com.tar.gz
  other-data/
    ...
```

Each poll cycle:

1. lists all `*.tar.gz` objects under the prefix
2. compares ETags against current state
3. downloads only changed archives
4. removes sites whose archives were deleted

## Hosted mode

With `-sites-config`, s3site serves declared sites only and does not bucket-scan.

Example config:

```json
{
  "sites": [
    { "hostname": "rohanverma.net" },
    { "hostname": "oddship.net" }
  ]
}
```

When `key` is omitted, it defaults to:

```txt
<prefix><hostname>.tar.gz
```

Hosted mode is intended for running behind a declarative ingress layer such as Caddy/Nix. CI uploads to a stable key and then asks the local control socket to refresh the site.

## Refresh control socket

The control socket is local-only and supports:

- `GET /health`
- `POST /refresh`

Typical usage:

```bash
s3site \
  -bucket static-assets \
  -prefix sites/ \
  -sites-config /etc/s3site/sites.json \
  -control-socket /run/s3site/control.sock \
  -listen 127.0.0.1:9001
```

Refresh a site after uploading a new tarball:

```bash
s3site refresh -socket /run/s3site/control.sock rohanverma.net
```

## Poll interval

For hosted mode on cost-sensitive object stores, use a long interval and rely on local refresh for the fast path.

For discovery mode, 15-30 seconds is reasonable for many setups.

## Admin host

The browser admin UI still exists, but it is intentionally gated behind `-allow-insecure-admin` because it has no authentication.

```bash
s3site -bucket b -admin-host admin.example.com -allow-insecure-admin -listen :8080
```

Do not expose that mode publicly without your own auth and network controls.

## Storage mode

By default, s3site keeps all site content in memory.

For systems where memory is constrained, use disk storage:

```bash
s3site -bucket b -storage disk -data-dir /var/lib/s3site
```

In disk mode, archives are extracted to versioned directories below the data directory and served via `os.DirFS`. s3site retains the previous version for one activation to reduce swap races.

| Mode | Pros | Cons |
|------|------|------|
| `memory` | Fast serving, no disk I/O | Uses RAM for all site content |
| `disk` | Minimal RAM usage | Disk I/O on every request, needs writable directory |

If `-data-dir` is not set, disk mode uses `$TMPDIR/s3site-data`.

## AWS credentials

s3site uses [simples3](https://github.com/rhnvrm/simples3), a minimal S3 client. Credentials are passed directly via flags or env vars -- there is no AWS config file or credential chain.

For IAM roles on EC2/ECS, you'll need to fetch temporary credentials and pass them in. For MinIO, just use the access/secret key pair.
