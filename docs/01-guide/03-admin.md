---
title: Admin UI
description: Manage sites from the browser
---

# Admin UI

s3site includes an admin interface for managing sites. It runs on a dedicated hostname, separate from your hosted sites.

## Enable it

Set the `-admin-host` flag:

```bash
s3site \
  -bucket my-bucket \
  -prefix sites/ \
  -admin-host admin.example.com \
  -listen :8080
```

The admin UI is available at `http://admin.example.com:8080/`. No admin routes are exposed on any other hostname.

## Endpoints

All endpoints are served only on the admin host.

### `GET /`

The admin UI. A single HTML page (styled with oat) that lists all sites and provides upload/delete controls. No JavaScript frameworks -- just fetch calls to the API below.

### `GET /api/sites`

Returns a JSON array of hostnames currently being served.

```bash
curl https://admin.example.com/api/sites
```

```json
["docs.example.com", "blog.example.com"]
```

Supports CORS (`Access-Control-Allow-Origin: *`) so other pages can fetch the site list.

### `POST /api/upload`

Upload a tar.gz archive as a multipart form. The archive is written to S3 and picked up on the next poll cycle.

```bash
curl -X POST https://admin.example.com/api/upload \
  -F hostname=docs.example.com \
  -F file=@site.tar.gz
```

Response:

```json
{"status": "ok", "hostname": "docs.example.com", "key": "sites/docs.example.com.tar.gz"}
```

### `POST /api/delete`

Delete a site's archive from S3. The site is removed on the next poll cycle.

```bash
curl -X POST https://admin.example.com/api/delete \
  -H 'Content-Type: application/json' \
  -d '{"hostname": "docs.example.com"}'
```

Response:

```json
{"status": "ok", "hostname": "docs.example.com"}
```

### `GET /health`

Returns `ok`. Use for load balancer health checks.

## CI/CD integration

Use the API from your CI pipeline to deploy on push:

```yaml
# GitHub Actions example
deploy:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - run: |
        hugo
        tar czf site.tar.gz -C public .
        curl -X POST https://admin.example.com/api/upload \
          -F hostname=docs.example.com \
          -F file=@site.tar.gz
```

Or skip the admin API and push directly to S3:

```yaml
    - run: aws s3 cp site.tar.gz s3://my-bucket/sites/docs.example.com.tar.gz
```

The admin API is convenient when you don't want to give CI direct S3 access.

## Security

The admin UI has no authentication. If you're exposing it on the internet, put it behind a reverse proxy with auth (HTTP basic, OAuth, VPN, etc.). In internal/staging environments, the lack of auth is usually fine.
