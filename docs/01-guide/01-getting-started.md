---
title: Getting Started
description: Install s3site and serve your first site
---

# Getting started

## Install

```bash
go install github.com/rhnvrm/s3site/cmd/s3site@latest
```

Or build from source:

```bash
git clone https://github.com/rhnvrm/s3site.git
cd s3site
go build -o s3site ./cmd/s3site/
```

For Linux servers (static binary):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o s3site ./cmd/s3site/
```

## Create a site archive

s3site serves tar.gz archives. The archive filename (minus `.tar.gz`) becomes the hostname. Files should be at the root of the archive, not wrapped in a directory.

```bash
# From a static site generator output
hugo && tar czf site.tar.gz -C public .

# Or from any directory of HTML files
tar czf site.tar.gz -C my-site .
```

## Upload to S3

```bash
aws s3 cp site.tar.gz s3://my-bucket/sites/docs.example.com.tar.gz
```

The key structure is: `{prefix}{hostname}.tar.gz`. With prefix `sites/` and hostname `docs.example.com`, the key is `sites/docs.example.com.tar.gz`.

## Run s3site

```bash
s3site \
  -bucket my-bucket \
  -prefix sites/ \
  -region us-east-1 \
  -listen :8080
```

Point DNS for `docs.example.com` at the server. The site is live.

## Using MinIO or S3-compatible storage

```bash
s3site \
  -bucket my-bucket \
  -prefix sites/ \
  -endpoint http://minio:9000 \
  -access-key minioadmin \
  -secret-key minioadmin \
  -listen :8080
```

## Update a site

Just upload a new archive with the same name. s3site detects the ETag change on the next poll and hot-reloads:

```bash
hugo && tar czf site.tar.gz -C public .
aws s3 cp site.tar.gz s3://my-bucket/sites/docs.example.com.tar.gz
# Site updates within one poll interval (default 30s)
```

## Remove a site

Delete the archive from S3:

```bash
aws s3 rm s3://my-bucket/sites/docs.example.com.tar.gz
# Site removed on next poll
```
