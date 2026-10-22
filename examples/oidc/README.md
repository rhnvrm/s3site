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
and checking one-use authorization codes and PKCE. Those fast tests do not contact Zitadel. Use the real-provider test below to
validate the container deployment, and check your production tenant settings
before deploying.

## Real Zitadel container integration

Run the complete real-provider integration test with:

```sh
./examples/oidc/test-zitadel.sh
```

This starts pinned Zitadel **v4.19.5**, the official **Login V2** image,
PostgreSQL, and S3Mock. It needs Docker Compose, curl, and a patched Go toolchain
(Go 1.26.10 or newer is recommended). Ports 18080 (Zitadel), 13000 (Login V2), and
19000 (S3Mock) must be free; they bind only to 127.0.0.1. The fixed test credentials
and master key are for this disposable local fixture only.

The harness creates real users, a project role, user grants, and a public PKCE
OIDC application. For **both memory and disk storage**, it uploads a site archive
and verifies:

- Verified email access succeeds; a user outside the email allowlist gets 403.
- A real project role grants access to `/docs`; a user without it gets 403.
- The issued subject ID can grant access independently of email or roles.
- Wrong passwords are rejected by Zitadel.
- The official Login V2 page renders for the authorization request.
- Password-backed sessions complete the authorization request; s3site exchanges
  the real code and verifies Zitadel-signed ID tokens using its published keys.
- Updating the S3 policy revokes the existing session's site access after refresh.

Automation uses Zitadel's **Session and OIDC APIs**, the same backend APIs used by
Login V2, instead of typing into the browser UI. It checks that the actual login
page renders but does not automate its browser interactions. No mocked tokens or
OIDC endpoints are used by this test. The separate fast tests still use the local
fixture for invalid signatures, expired tokens, and other negative cases.

Bootstrap PATs are copied to private temporary files without printing their
contents. The harness removes the containers, database/bootstrap volumes, and
temporary tokens on exit. It uses the dedicated Compose project
`s3site-zitadel`; do not use that project for persistent development data.
The same command runs in PR CI as **Zitadel integration**.
