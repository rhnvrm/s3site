# Local OIDC login demo

Requirements: Go, Docker Compose, curl, base64, and tar. No Zitadel tenant or AWS
account is needed. Run from the repository root:

```sh
./examples/oidc/run.sh
```

Open **http://127.0.0.1:8080** (use that exact address, not localhost), then follow
the private-site link. The local provider shows a sign-in form:

- Alice: verified, allowed email; opens the private S3-hosted page.
- Bob: verified, outside the allowlist; receives 403.
- Alice with unverified email: receives 403.

Use the sign-out button or clear site cookies to try another user. The provider
is intentionally passwordless and binds only to loopback. Never deploy it as an
identity service. Adobe S3Mock supplies an ephemeral S3-compatible HTTP service on
127.0.0.1:19000; the provider uses 127.0.0.1:5556. Ctrl-C stops the processes,
removes generated configuration and keys, and removes the demo container.

The demo builds the actual s3site binary, packages the example site, uploads its
archive over HTTP, and runs the production OIDC handler. To use real Zitadel,
follow [the configuration guide](../../docs/access-control.md#zitadel-setup).

## Automated tests

Local OIDC flow tests run without Docker:

```sh
go test ./... -short -race -count=1
```

To run S3-backed integration tests separately:

```sh
docker compose -p s3site-oidc-demo -f examples/oidc/compose.yml up -d
# Wait until this succeeds:
curl -fsS http://127.0.0.1:19000/testbucket
AWS_S3_ENDPOINT=http://127.0.0.1:19000 go test ./... -race -count=1
docker compose -p s3site-oidc-demo -f examples/oidc/compose.yml down
```

The full suite also works against an existing MinIO instance using the repository's
`AWS_S3_*` settings. The OIDC S3 test uses isolated object keys and covers memory
and disk extraction, a complete login, failed deployment rollback, and access
revocation after refresh. All OIDC tests use a local fixture signing real JWTs
and checking one-use authorization codes and PKCE. They do not contact live
Zitadel; test your tenant configuration before deploying.
