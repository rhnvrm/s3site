---
title: Deployment
description: Running s3site in production
---

# Deployment

s3site compiles to a single static binary.

## Reverse proxy

In production, put s3site behind a reverse proxy (nginx, HAProxy, Caddy) for TLS and hostname routing.

### Wildcard subdomains

A typical setup is `s3site-{name}.example.com` with a wildcard DNS record and TLS cert:

```
*.example.com -> reverse proxy -> s3site
```

The reverse proxy matches hostnames starting with a prefix and forwards to s3site. s3site routes by the full hostname.

### HAProxy example

```
# ACL matches s3site-* subdomains
acl is_s3site hdr_beg(host) -i s3site-
use_backend s3site if is_s3site

backend s3site
    server s3site-1 127.0.0.1:8080
```

### Caddy example

```
s3site-*.example.com {
    reverse_proxy localhost:8080
}
```

## Nomad

s3site works well with Nomad's `exec` driver. Use a template block for configuration and service discovery:

```hcl
job "s3site" {
  group "s3site" {
    network {
      port "http" {}
    }

    service {
      name = "s3site"
      port = "http"
      provider = "nomad"

      check {
        type     = "tcp"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "s3site" {
      driver = "exec"

      artifact {
        source = "https://your-artifact-store/s3site"
      }

      template {
        data = <<-EOF
          AWS_S3_BUCKET=my-bucket
          AWS_S3_REGION=us-east-1
          S3SITE_PREFIX=sites/
          S3SITE_POLL=15s
          S3SITE_ADMIN_HOST=admin.example.com
        EOF
        destination = "secrets/env"
        env         = true
      }

      config {
        command = "$${NOMAD_TASK_DIR}/s3site"
        args    = ["-listen", ":$${NOMAD_PORT_http}"]
      }
    }
  }
}
```

Use dynamic ports (`port "http" {}`) so Nomad picks a free port. The service registration tells your load balancer where to find it.

## Docker

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /s3site ./cmd/s3site/

FROM scratch
COPY --from=build /s3site /s3site
ENTRYPOINT ["/s3site"]
```

```bash
docker run -p 8080:8080 s3site \
  -bucket my-bucket \
  -prefix sites/ \
  -listen :8080
```

## systemd

```ini
[Unit]
Description=s3site static site server
After=network.target

[Service]
ExecStart=/usr/local/bin/s3site -listen :8080
Environment=AWS_S3_BUCKET=my-bucket
Environment=AWS_S3_REGION=us-east-1
Environment=S3SITE_PREFIX=sites/
Environment=S3SITE_POLL=30s
Environment=S3SITE_ADMIN_HOST=admin.example.com
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## Resource usage

In memory mode (default), all site content lives in RAM. Plan for the sum of all uncompressed archives plus ~20% overhead. CPU is negligible -- just HTTP serving and periodic S3 polls. A few small doc sites need 64-256 MB.

In disk mode (`-storage disk`), sites are extracted to disk. RAM stays around 10-20 MB regardless of how many sites you host. The tradeoff is disk I/O on every request, though the OS page cache helps for frequently accessed files.

Use disk mode when you have many or large sites and don't want to provision memory for all of them.
