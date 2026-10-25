#!/usr/bin/env bash
set -euo pipefail

image="${HAKO_API_SMOKE_IMAGE:-hako-api:rie-local}"
database_url="${HAKO_DATABASE_URL:-}"
port="${HAKO_API_RIE_SMOKE_PORT:-8080}"
container_name="hako-api-rie-smoke-$$"
response_file="$(mktemp)"

if [[ -z "$database_url" ]]; then
  echo "set HAKO_DATABASE_URL to a disposable local PostgreSQL database reachable from Podman" >&2
  exit 2
fi
for command in podman curl python3; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "$command is required" >&2
    exit 2
  fi
done
if [[ "$(podman info --format '{{.Host.Arch}}')" != amd64 ]]; then
  echo "the RIE smoke image currently requires an amd64 Podman host" >&2
  exit 2
fi
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || (( port < 1024 || port > 65535 )); then
  echo "HAKO_API_RIE_SMOKE_PORT must be between 1024 and 65535" >&2
  exit 2
fi
if ! python3 - "$port" <<'PY'
import socket
import sys

with socket.socket() as probe:
    try:
        probe.bind(("127.0.0.1", int(sys.argv[1])))
    except OSError as error:
        raise SystemExit(f"HAKO_API_RIE_SMOKE_PORT is unavailable: {error}")
PY
then
  exit 2
fi

cleanup() {
  podman rm --force "$container_name" >/dev/null 2>&1 || true
  rm -f "$response_file"
}
trap cleanup EXIT INT TERM

# The test-only image uses the pinned Lambda provided base, whose entrypoint starts RIE locally.
podman run --detach --name "$container_name" --network host \
  --env HAKO_DATABASE_URL="$database_url" \
  --env HAKO_COGNITO_ISSUER=https://cognito-idp.ap-northeast-1.amazonaws.com/ap-northeast-1_RIESmoke \
  --env HAKO_COGNITO_CLIENT_ID=hako-api-rie-smoke \
  "$image" >/dev/null

ready=false
for _ in $(seq 1 60); do
  if ! podman container exists "$container_name"; then
    echo "API Lambda image exited before RIE became ready" >&2
    podman logs "$container_name" >&2 || true
    exit 1
  fi
  if curl --silent --show-error --max-time 2 "http://127.0.0.1:${port}/2015-03-31/functions/function/invocations" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo "RIE did not become ready within 60 seconds" >&2
  podman logs "$container_name" >&2 || true
  exit 1
fi

invoke_and_check() {
  local path="$1"
  local expected_status="$2"
  local event
  event="$(python3 - "$path" <<'PY'
import json
import sys

path = sys.argv[1]
event = {
    "version": "2.0",
    "routeKey": f"GET {path}",
    "rawPath": path,
    "rawQueryString": "",
    "headers": {"host": "api.example.test", "user-agent": "hako-rie-smoke"},
    "requestContext": {
        "accountId": "123456789012",
        "apiId": "hako-smoke",
        "domainName": "api.example.test",
        "domainPrefix": "api",
        "http": {"method": "GET", "path": path, "protocol": "HTTP/1.1", "sourceIp": "127.0.0.1", "userAgent": "hako-rie-smoke"},
        "requestId": "hako-rie-smoke-request",
        "routeKey": f"GET {path}",
        "stage": "$default",
        "time": "25/Oct/2026:00:00:00 +0000",
        "timeEpoch": 1792886400000,
    },
    "isBase64Encoded": False,
}
print(json.dumps(event, separators=(",", ":")))
PY
)"
  curl --silent --show-error --fail-with-body --max-time 20 \
    --output "$response_file" --request POST \
    --header 'content-type: application/json' --data "$event" \
    "http://127.0.0.1:${port}/2015-03-31/functions/function/invocations"
  python3 - "$response_file" "$path" "$expected_status" <<'PY'
import json
import sys

filename, path, expected = sys.argv[1], sys.argv[2], int(sys.argv[3])
with open(filename, encoding="utf-8") as response_file:
    result = json.load(response_file)
actual = result.get("statusCode")
if actual != expected:
    raise SystemExit(f"{path}: expected statusCode {expected}, received {actual}: {result!r}")
print(f"API Gateway v2 {path}: statusCode={actual}")
PY
}

invoke_and_check /healthz 200
invoke_and_check /v1/health 401
