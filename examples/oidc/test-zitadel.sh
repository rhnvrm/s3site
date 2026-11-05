#!/usr/bin/env bash
# Disposable local containers only; credentials never leave temporary files.
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/seaweedfs-env.sh
run_dir=$(mktemp -d)
compose=(docker compose -p s3site-zitadel -f examples/oidc/compose.yml -f examples/oidc/compose.zitadel.yml)
cleanup() {
  result=$?
  if [[ $result -ne 0 ]]; then "${compose[@]}" logs --tail 80 s3 zitadel zitadel-login >&2 || true; fi
  "${compose[@]}" down --volumes --remove-orphans >/dev/null
  rm -rf "$run_dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
"${compose[@]}" up -d
bash scripts/seaweedfs-wait.sh
ready=false
for ((i=0;i<120;i++)); do
  if curl -fsS http://127.0.0.1:18080/.well-known/openid-configuration >/dev/null 2>&1 &&
     curl -fsS http://127.0.0.1:13000/ui/v2/login/healthy >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then echo 'Zitadel test containers did not become ready' >&2; exit 1; fi
"${compose[@]}" cp zitadel:/bootstrap/admin.pat "$run_dir/admin.pat"
"${compose[@]}" cp zitadel:/bootstrap/login-client.pat "$run_dir/login-client.pat"
chmod 600 "$run_dir"/*.pat
export S3SITE_ZITADEL_ISSUER=http://127.0.0.1:18080
export S3SITE_ZITADEL_ADMIN_PAT_FILE="$run_dir/admin.pat"
export S3SITE_ZITADEL_LOGIN_PAT_FILE="$run_dir/login-client.pat"
export S3SITE_ZITADEL_LOGIN_UI=true
go test ./pkg/s3site -run '^TestZitadelIntegration$' -race -count=1 -v "$@"
