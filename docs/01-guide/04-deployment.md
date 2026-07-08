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

Container images are published to GitHub Container Registry on version tags:

- `ghcr.io/rhnvrm/s3site:v0.1.0`
- `ghcr.io/rhnvrm/s3site:latest`

```bash
docker run -p 8080:80 ghcr.io/rhnvrm/s3site:v0.1.0 \
  -bucket my-bucket \
  -prefix sites/
```

The published image defaults to `-listen :80`, which fits ALB and Nomad setups that route HTTP to port 80. Override `-listen` if you want a different internal port.

The repository also includes a `Dockerfile` if you want to build it yourself.

## Release flow

Create and push a semver tag to publish both binary assets and the container image:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The GitHub Actions release workflow will:
- run tests
- publish tar.gz binaries on GitHub Releases
- publish a multi-arch image to GHCR

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
