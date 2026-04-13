---
title: Admin UI
description: Development-only browser admin
---

# Admin UI

s3site still includes a browser admin interface for development, but it is **not** the recommended production control plane.

For hosted deployments, prefer:

- stable object keys per site
- local refresh via unix socket
- CI or SSH invoking `s3site refresh`

## Enable it

The admin UI is intentionally gated behind `-allow-insecure-admin` because it has no authentication.

```bash
s3site \
  -bucket my-bucket \
  -prefix sites/ \
  -admin-host admin.example.com \
  -allow-insecure-admin \
  -listen :8080
```

The admin UI is available at `http://admin.example.com:8080/`. No admin routes are exposed on any other hostname.

## Endpoints

All endpoints are served only on the admin host.

### `GET /`

A single HTML page that lists loaded sites and provides upload/delete controls.

### `GET /api/sites`

Returns a JSON array of hostnames currently being served.

### `POST /api/upload`

Upload a tar.gz archive as a multipart form. The archive is written to S3 and picked up on the next poll cycle.

### `POST /api/delete`

Delete a site's archive from S3. The site is removed on the next poll cycle in discovery mode.

### `GET /health`

Returns `ok`.

## Security

The admin UI has no authentication and should be treated as insecure.

Use it only when you control access through:

- private networking
- VPN/Tailscale
- reverse-proxy auth
- local development

For production hosted mode, do not expose this API publicly. Use the unix control socket and `s3site refresh` instead.
