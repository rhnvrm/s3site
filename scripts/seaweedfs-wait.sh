#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/seaweedfs-env.sh

# Check signed bucket access so readiness includes identity and bucket seeding.
for attempt in {1..60}; do
  if curl -fsS --connect-timeout 2 --max-time 4 \
    --aws-sigv4 "aws:amz:$AWS_S3_REGION:s3" \
    --user "$AWS_S3_ACCESS_KEY:$AWS_S3_SECRET_KEY" \
    --head "$AWS_S3_ENDPOINT/$AWS_S3_BUCKET" >/dev/null 2>&1; then
    echo "SeaweedFS is ready at $AWS_S3_ENDPOINT"
    exit 0
  fi
  sleep 1
done
echo 'SeaweedFS S3 bucket did not become ready' >&2
exit 1
