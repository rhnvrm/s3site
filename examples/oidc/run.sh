#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
run_dir=$(mktemp -d)
provider_pid=""
site_pid=""
compose=(docker compose -p s3site-oidc-demo -f examples/oidc/compose.yml)
cleanup() {
  if [[ -n "$site_pid" ]]; then kill "$site_pid" 2>/dev/null || true; fi
  if [[ -n "$provider_pid" ]]; then kill "$provider_pid" 2>/dev/null || true; fi
  "${compose[@]}" down >/dev/null
  rm -rf "$run_dir"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

go build -o "$run_dir/s3site" ./cmd/s3site
go build -o "$run_dir/provider" ./cmd/oidc-test-provider
"${compose[@]}" up -d s3
ready=false
for ((i=0;i<60;i++)); do
  if curl -fsS http://127.0.0.1:19000/testbucket >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
if [[ "$ready" != true ]]; then echo 'S3Mock did not become ready' >&2; exit 1; fi
tar -czf "$run_dir/site.tar.gz" -C examples/oidc/site .
curl -fsS -X PUT --data-binary "@$run_dir/site.tar.gz" http://127.0.0.1:19000/testbucket/127.0.0.1.tar.gz
cookie_key=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
cat > "$run_dir/oidc.json" <<JSON
{"issuer":"http://127.0.0.1:5556","clientId":"s3site-local","cookieKey":"$cookie_key","redirectUrls":["http://127.0.0.1:8080/_s3site/callback"],"allowLocalHttp":true}
JSON
chmod 600 "$run_dir/oidc.json"
"$run_dir/provider" &
provider_pid=$!
for ((i=0;i<60;i++)); do
  if curl -fsS http://127.0.0.1:5556/.well-known/openid-configuration >/dev/null 2>&1; then break; fi
  sleep 1
done
AWS_S3_ENDPOINT=http://127.0.0.1:19000 AWS_S3_ACCESS_KEY=minioadmin AWS_S3_SECRET_KEY=minioadmin \
  "$run_dir/s3site" -bucket testbucket -listen 127.0.0.1:8080 -oidc-config "$run_dir/oidc.json" &
site_pid=$!
echo 'Open http://127.0.0.1:8080 and follow the private site link. Ctrl-C stops the demo.'
wait "$site_pid"
