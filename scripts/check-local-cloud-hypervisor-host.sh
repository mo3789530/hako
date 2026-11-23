#!/bin/sh
set -eu

cloud_hypervisor_bin=${CLOUD_HYPERVISOR_BIN:-cloud-hypervisor}
if ! command -v "$cloud_hypervisor_bin" >/dev/null 2>&1; then
  echo "Cloud Hypervisor binary not found: $cloud_hypervisor_bin" >&2
  exit 1
fi
if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
  echo "readable and writable /dev/kvm is required" >&2
  exit 1
fi
"$cloud_hypervisor_bin" --version
python3 - <<'PY'
import fcntl
import os
fd = os.open('/dev/kvm', os.O_RDWR | os.O_CLOEXEC)
try:
    version = fcntl.ioctl(fd, 0xAE00, 0)
finally:
    os.close(fd)
if version != 12:
    raise SystemExit(f'unsupported KVM API version: {version}')
print('local Cloud Hypervisor KVM host preflight passed')
PY
