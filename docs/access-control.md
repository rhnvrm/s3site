# Site access policies and OIDC

Put `.s3site/access.json` at the root of a site's tar.gz archive. s3site loads
and validates it alongside the content, then activates both together. A site
without this file remains public. A present but invalid policy rejects the new
archive; the last valid deployment remains active. A new site with an invalid
policy is not activated. Removing the file deliberately makes the site public.
The whole `.s3site` directory is reserved and never served or listed.

## Whole-site allowlist

```json
{
  "version": 1,
  "auth": "oidc",
  "allow": {
    "emails": ["alice@example.com"],
    "subjects": ["stable-provider-user-id"],
    "roles": ["docs-reader"]
  }
}
```

An allowlist grants access if **any** listed email, subject, or role matches.
Email matching is case insensitive and requires `email_verified: true` in the
verified ID token. Subjects and roles match exactly, including case. Subject IDs
belong to the single issuer configured by the operator. An empty allowlist denies
everyone. Passwords, client secrets, and provider settings do not belong here.

## Path rules

Use `rules` instead of `allow` to mix public pages, user grants, and role grants:

```json
{
  "version": 1,
  "auth": "oidc",
  "rules": [
    { "path": "/", "public": true },
    { "path": "/index.html", "public": true },
    { "pathPrefix": "/assets", "public": true },
    { "pathPrefix": "/docs/admin", "allow": { "roles": ["docs-admin"] } },
    { "pathPrefix": "/docs", "allow": { "roles": ["docs-reader", "docs-admin"] } },
    { "path": "/preview.html", "allow": { "emails": ["alice@example.com"] } }
  ]
}
```

- Rules are ordered: **the first matching rule wins**. Put exceptions first.
- Unmatched paths are denied, including for authenticated users. `rules: []`
  denies everything. Omitting both `allow` and `rules` also denies everything.
- Each rule has exactly one of `path` and `pathPrefix`, plus either `public: true`
  or `allow`. An empty rule allowlist is an explicit deny, useful as an exception.
- `path` matches an exact normalized, URL-decoded path. A trailing slash is
  equivalent to no trailing slash. Dot segments and repeated slashes normalize
  before matching. Query parameters do not affect permission.
- `pathPrefix: "/docs"` matches `/docs`, `/docs/`, and descendants, but not
  `/docs-other`. `/` matches everything. Prefixes can end in `/`.
- Patterns and regular expressions are not supported. Policy paths must already
  be clean, absolute, decoded paths, without query strings or fragments.
- Both the requested path **and the actual representation** must allow access.
  For example, `/docs/` also checks `/docs/index.html`; a missing URL using a
  custom error page also checks `/404.html`. Use prefix rules for directories.
- Policies cover assets, downloads, HEAD, range and conditional requests too.
  Directory listings are disabled on sites with a policy to avoid exposing
  protected filenames. Put an `index.html` in directories you intend to serve.
- `/_s3site/callback` and `/_s3site/logout` are reserved authentication routes.
  Do not use that namespace for site content or access rules.

Unknown JSON fields, unsupported versions, invalid rules, policies larger than
64 KiB, and extra JSON values are rejected. See
[access.schema.json](access.schema.json) for editor/tooling validation. Runtime
validation additionally checks clean paths and identity values.

## Operator configuration

Pass `-oidc-config /etc/s3site/oidc.json` or set `S3SITE_OIDC_CONFIG`:

```json
{
  "issuer": "https://your-instance.zitadel.cloud",
  "clientId": "your-client-id",
  "cookieKey": "BASE64_ENCODED_32_RANDOM_BYTES",
  "redirectUrls": ["https://docs.example.com/_s3site/callback"]
}
```

Use Go 1.26.10 or a newer patched toolchain for production builds.

Generate a cookie key with `openssl rand -base64 32`. Protect this configuration
file as a secret. Optional `clientSecret` supports confidential clients; omitting
it uses a public PKCE client. The issuer is discovered at startup; discovery
failure stops startup. Protected requests fail closed with 503 if OIDC or the
site's exact callback registration is missing. Public paths still work.

Register every callback URL both here and at the provider. Callback authorities,
including ports, are fixed and checked against the request Host. Forwarded
headers never choose callback origins. Configure your reverse proxy to preserve
the external Host, terminate HTTPS, and prevent direct untrusted access to the
backend. There is one callback per hostname and one provider/client per process.

All provider and callback URLs require HTTPS. For development only,
`allowLocalHttp: true` permits HTTP on `localhost`, `127.0.0.1`, or `::1`.

### Zitadel setup

1. Create an OIDC application with Authorization Code + PKCE and authentication
   method **none**, and register the exact callback URLs. For a confidential
   application, supply its secret instead.
2. Set the issuer to your Zitadel instance URL and copy the client ID.
3. Include email claims in the ID token for email allowlists. Only verified
   emails grant access.
4. For role grants, assign users the appropriate project roles and enable user
   roles in the **ID token** in application settings. Configure `rolesClaim` to
   select the precise project claim, for example:

```json
{
  "rolesClaim": "urn:zitadel:iam:org:project:123456:roles",
  "scopes": ["urn:zitadel:iam:org:project:id:123456:aud", "urn:zitadel:iam:org:projects:roles"]
}
```

Add these fields to the operator configuration. Additional scopes supplement
`openid email`. See Zitadel's [role guide](https://zitadel.com/docs/guides/integrate/retrieve-user-roles)
and [application settings](https://zitadel.com/docs/guides/manage/console/projects-overview).
The chosen claim must actually appear in the ID token; s3site does not fetch
UserInfo or accept access tokens as identity tokens.

`rolesClaim` accepts a string array or a Zitadel role map of
`{"role-key":{"organization-id":"organization-domain"}}`. A role-map key grants
the role if present for any organization in that selected claim. Use a
project-specific claim, and a custom string-array claim if you need to distinguish
organizations. Roles are ignored unless the operator explicitly selects a claim.
Site archives cannot select a provider, project claim, or trusted header.

## Login and session behavior

A browser GET to an allowed but protected path redirects to the identity
provider and returns to the original same-site path after login. JSON requests
(`Accept: application/json`) and unauthenticated non-GET requests receive 401.
Authenticated visitors missing the required grant receive 403. Unmatched paths
receive 403 without starting login.

Authorization code flow uses PKCE S256, state, and a nonce. Token signature,
issuer, audience, expiry, and nonce are validated. Login transactions expire in
five minutes. Encrypted, authenticated session cookies are bound to the hostname,
callback, issuer, client, selected role claim, and cookie purpose. Production
cookies use `__Host-`, Secure, HttpOnly, SameSite=Lax, Path=/ and no Domain.

Sessions last at most eight hours, capped by ID token expiry. There is no refresh
token or server-side session database. All replicas must share the same cookie
key and configuration. Rotating the key invalidates existing sessions. Role and
identity changes at the provider take effect on the next login/session expiry;
site allowlist changes take effect on the next successful archive refresh, even
for existing sessions. Provider account revocation is not immediately propagated.

Responses from policy sites with OIDC use `Cache-Control: private, no-store`.
Configure upstream CDNs to bypass caching for protected sites, and purge any
previously public cached content when enabling protection. Previously downloaded
content cannot be recalled. Keep S3 objects private so visitors cannot bypass
s3site by fetching archives directly.

POST `/_s3site/logout` with the site's Origin header clears its local session.
It does not sign the visitor out of Zitadel or other sites. The next protected
visit can sign in again through an existing provider session.

## Testing

See [the local demo](../examples/oidc/README.md). Automated tests use a local
signed-token OIDC fixture; they require no external account. They cover the
redirect, sign-in form, code exchange, PKCE, token verification, cookies, path
rules, email verification, roles, denial, logout, and live policy changes.
S3 integration tests also upload, extract, and refresh real tar.gz objects over
HTTP in both disk and memory modes. Run `./examples/oidc/test-zitadel.sh` for an additional real-provider suite using
Zitadel v4.19.5, its official Login V2 container, PostgreSQL, and S3Mock. It tests
real password checks, authorization codes, signed ID tokens, verified email and
project role grants, subject IDs, denial and live policy revocation in both
storage modes. The official login page is fetched; automated password entry and
OIDC completion use Zitadel's Session and OIDC APIs rather than browser clicks.
The containers, bootstrap credentials, and database are disposable. PR CI runs
this suite separately from the fast fixture tests.
