---
title: s3site
layout: landing
---

# s3site

Serve static websites from tar.gz archives in S3. Zero restarts.

Push a tar.gz named after a hostname to an S3 bucket. s3site picks it up and serves it. When the archive changes, the site hot-reloads. When it's deleted, the site disappears.

```
s3://my-bucket/sites/
  docs.example.com.tar.gz     -> serves docs.example.com
  blog.example.com.tar.gz     -> serves blog.example.com
```

## How it works

1. Lists the S3 bucket for `*.tar.gz` files under a configured prefix
2. Downloads each archive, extracts into an in-memory `fs.FS`
3. Routes HTTP requests by `Host` header to the matching site
4. Polls for changes -- compares ETags, re-downloads only when changed
5. Removes sites whose archives are deleted from S3

All files are served from memory. No disk I/O after startup.

## Install

```bash
go install github.com/rhnvrm/s3site/cmd/s3site@latest
```

## Quick start

```bash
# Build your site, pack it, push to S3
tar czf site.tar.gz -C public .
aws s3 cp site.tar.gz s3://my-bucket/sites/docs.example.com.tar.gz

# Run s3site
s3site -bucket my-bucket -prefix sites/ -listen :8080
```

Point `docs.example.com` at the s3site server and your site is live.
