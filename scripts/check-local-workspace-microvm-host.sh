#!/bin/sh
set -eu

if [ "$(uname -s)" != "Linux" ]; then
  echo "local Workspace MicroVM requires a Linux host" >&2
  exit 1
fi

if [ ! -c /dev/kvm ]; then
  echo "/dev/kvm is not available" >&2
  exit 1
fi

if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
  echo "/dev/kvm must be readable and writable by the current user" >&2
  exit 1
fi

firecracker_bin=${FIRECRACKER_BIN:-firecracker}
if ! command -v "$firecracker_bin" >/dev/null 2>&1; then
  echo "Firecracker binary not found: $firecracker_bin" >&2
  exit 1
fi

"$firecracker_bin" --version
