#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
compose=(docker compose -p "${COMPOSE_PROJECT_NAME:-s3site-test}" -f examples/oidc/compose.yml)
"${compose[@]}" up -d s3
if ! bash scripts/seaweedfs-wait.sh; then
  "${compose[@]}" logs --tail 100 s3
  exit 1
fi
