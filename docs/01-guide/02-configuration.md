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
| `-admin-host` | `S3SITE_ADMIN_HOST` | | Hostname for admin UI |
| `-storage` | `S3SITE_STORAGE` | `memory` | Storage mode: `memory` or `disk` |
| `-data-dir` | `S3SITE_DATA_DIR` | `$TMPDIR/s3site-data` | Directory for disk storage |

Environment variables take effect only when the flag is not set. Flags always win.

## Prefix

The prefix scopes s3site to a subdirectory in the bucket. This lets you share a bucket with other data:

```
s3://shared-bucket/
  sites/                      <- s3site watches here with -prefix sites/
    foo.example.com.tar.gz
    bar.example.com.tar.gz
  other-data/
    ...
```

Without a prefix, s3site watches the entire bucket for `.tar.gz` files.

## Poll interval

s3site polls S3 for changes at the configured interval. Each poll cycle:

1. Lists all `*.tar.gz` objects under the prefix
2. Compares ETags against in-memory state
3. Downloads only changed archives (new or updated ETag)
4. Removes sites whose archives were deleted

For most use cases, 15-30 seconds is reasonable. S3 LIST requests are cheap.

## Admin host

When `-admin-host` is set, requests to that hostname are routed to the admin UI instead of the site handler. This keeps admin endpoints off your hosted sites.

```bash
s3site -bucket b -admin-host admin.example.com -listen :8080
```

See [Admin UI](../admin/) for details.

## Storage mode

By default, s3site keeps all site content in memory. This is fast but uses RAM proportional to the total uncompressed size of all sites.

For systems where memory is constrained, use disk storage:

```bash
s3site -bucket b -storage disk -data-dir /var/lib/s3site
```

In disk mode, archives are extracted to `{data-dir}/{hostname}/` and served via `os.DirFS`. Each request reads from disk. When a site is updated, the new version is extracted to a temp directory first, then atomically swapped in.

| Mode | Pros | Cons |
|------|------|------|
| `memory` | Fast serving, no disk I/O | Uses RAM for all site content |
| `disk` | Minimal RAM usage | Disk I/O on every request, needs writable directory |

If `-data-dir` is not set, disk mode uses `$TMPDIR/s3site-data`.

## AWS credentials

s3site uses [simples3](https://github.com/rhnvrm/simples3), a minimal S3 client. Credentials are passed directly via flags or env vars -- there is no AWS config file or credential chain.

For IAM roles on EC2/ECS, you'll need to fetch temporary credentials and pass them in. For MinIO, just use the access/secret key pair.
