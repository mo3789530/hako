#!/usr/bin/env bash
set -euo pipefail

image="${HAKO_WORKSPACE_NETWORK_TEST_IMAGE:-docker.io/library/alpine@sha256:dc92f017beea8e7f43263073d717f7bc011228c95d5312e440deb4d035bb4eec}"
control_network="${HAKO_WORKSPACE_NETWORK_CONTROL:-podman}"
internal_network="hako-ws-egress-$$"

if ! command -v podman >/dev/null 2>&1; then
	echo "podman is required" >&2
	exit 1
fi

if ! podman image exists "$image"; then
	podman pull --platform linux/amd64 "$image" >/dev/null
fi
image_platform="$(podman image inspect "$image" --format '{{.Os}}/{{.Architecture}}')"
if [[ "$image_platform" != "linux/amd64" ]]; then
	echo "network test image must match the local x86_64 Podman runtime" >&2
	exit 1
fi

control_internal="$(podman network inspect "$control_network" --format '{{.Internal}}')"
if [[ "$control_internal" != "false" ]]; then
	echo "control network must allow external egress" >&2
	exit 1
fi

cleanup() {
	podman network rm "$internal_network" >/dev/null 2>&1 || true
}
trap cleanup EXIT
podman network create --internal "$internal_network" >/dev/null

echo "Checking positive egress control on network $control_network"
podman run --rm --pull=never --network "$control_network" "$image" \
	busybox nc -z -w 5 1.1.1.1 80

echo "Checking blocked egress on internal network $internal_network"
if podman run --rm --pull=never --network "$internal_network" "$image" \
	busybox nc -z -w 5 1.1.1.1 80; then
	echo "internal Podman network unexpectedly reached the external probe" >&2
	exit 1
fi

internal_flag="$(podman network inspect "$internal_network" --format '{{.Internal}}')"
if [[ "$internal_flag" != "true" ]]; then
	echo "created Podman network did not retain Internal=true" >&2
	exit 1
fi

echo "Podman internal-network egress smoke test passed"
