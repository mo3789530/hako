#!/usr/bin/env bash
set -euo pipefail

image="${HAKO_API_SMOKE_IMAGE:-hako-api:smoke}"
database_url="${HAKO_DATABASE_URL:-}"
port="${HAKO_API_SMOKE_PORT:-18080}"
container_name="hako-api-smoke-$$"

if [[ -z "$database_url" ]]; then
  echo "set HAKO_DATABASE_URL to a disposable local PostgreSQL database" >&2
  exit 2
fi
if ! command -v podman >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "podman and curl are required" >&2
  exit 2
fi
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || (( port < 1024 || port > 65535 )); then
  echo "HAKO_API_SMOKE_PORT must be between 1024 and 65535" >&2
  exit 2
fi

cleanup() {
  podman rm --force "$container_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

podman run --detach --rm --name "$container_name" --network host \
  --env HAKO_DATABASE_URL="$database_url" \
  --env HAKO_COGNITO_ISSUER=https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_SmokeTest \
  --env HAKO_COGNITO_CLIENT_ID=hako-api-smoke \
  --env HAKO_API_HTTP_MODE=true \
  --env HAKO_API_LISTEN_ADDR="127.0.0.1:${port}" \
  "$image" >/dev/null

ready=false
for _ in $(seq 1 60); do
  if ! podman container exists "$container_name"; then
    echo "API image exited before becoming ready" >&2
    podman logs "$container_name" >&2 || true
    exit 1
  fi
  if curl --silent --show-error --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "API OCI image did not become ready within 60 seconds" >&2
  podman logs "$container_name" >&2 || true
  exit 1
fi

public_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 5 "http://127.0.0.1:${port}/healthz")"
private_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --max-time 5 "http://127.0.0.1:${port}/v1/health")"
if [[ "$public_status" != 200 || "$private_status" != 401 ]]; then
  echo "unexpected health status: /healthz=${public_status}, /v1/health=${private_status}" >&2
  podman logs "$container_name" >&2 || true
  exit 1
fi

echo "API OCI image smoke passed: /healthz=${public_status}, unauthenticated /v1/health=${private_status}"
