#!/bin/sh
set -eu

case "$(uname -m)" in
  x86_64) host_arch=amd64; other_arch=arm64 ;;
  aarch64|arm64) host_arch=arm64; other_arch=amd64 ;;
  *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac
for tool in go python3 cpio gzip; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "required initrd packaging test tool not found: $tool" >&2
    exit 1
  fi
done

test_dir=$(mktemp -d "${TMPDIR:-/tmp}/hako-initrd-package-test.XXXXXX")
cleanup() {
  if [ -d "$test_dir" ]; then
    find "$test_dir" -type d -exec chmod u+rwx {} +
    rm -rf "$test_dir"
  fi
}
trap cleanup EXIT HUP INT TERM
rootfs="$test_dir/rootfs"
mkdir -p "$rootfs/usr/sbin" "$rootfs/usr/local/bin" \
  "$rootfs/opt/hako-code-server/bin" "$rootfs/etc/hako"
mkdir -p "$rootfs/opt/read-only-test"
touch "$rootfs/opt/read-only-test/payload"
chmod 0555 "$rootfs/opt/read-only-test"
GOOS=linux GOARCH="$host_arch" CGO_ENABLED=0 go build -trimpath -o "$test_dir/guest-init" ./cmd/hako-local-workspace-guest-init
cp "$test_dir/guest-init" "$rootfs/usr/sbin/sshd"
cp "$test_dir/guest-init" "$rootfs/usr/local/bin/hako-workspace-agent"
cp "$test_dir/guest-init" "$rootfs/opt/hako-code-server/bin/code-server"
printf '{"keys":[]}\n' > "$rootfs/etc/hako/workspace-agent-public-keys.json"
scripts/package-local-workspace-initrd.sh "$rootfs" "$test_dir/guest-init" "$test_dir/workspace-initrd.cpio.gz"
gzip -dc "$test_dir/workspace-initrd.cpio.gz" | cpio --quiet --list > "$test_dir/archive.list"
for required in init usr/sbin/sshd usr/local/bin/hako-workspace-agent \
  opt/hako-code-server/bin/code-server etc/hako/workspace-agent-public-keys.json; do
  if ! grep -Fxq "$required" "$test_dir/archive.list"; then
    echo "initrd archive did not contain $required" >&2
    exit 1
  fi
done
python3 - "$test_dir/workspace-initrd.cpio.gz" <<'PY'
import gzip, stat, sys

with gzip.open(sys.argv[1], 'rb') as archive:
    data = archive.read()
offset = 0
root_mode = None
tmp_mode = None
while offset + 110 <= len(data):
    header = data[offset:offset + 110]
    if header[:6] not in (b'070701', b'070702'):
        raise SystemExit('initrd contains an invalid newc entry')
    fields = [int(header[6 + index * 8:14 + index * 8], 16) for index in range(13)]
    name_size, file_size = fields[11], fields[6]
    name_start = offset + 110
    name = data[name_start:name_start + name_size - 1]
    file_start = (name_start + name_size + 3) & ~3
    offset = (file_start + file_size + 3) & ~3
    if name == b'.':
        root_mode = fields[1]
    elif name == b'tmp':
        tmp_mode = fields[1]
    if name == b'TRAILER!!!':
        break
if root_mode is None or not stat.S_ISDIR(root_mode) or stat.S_IMODE(root_mode) != 0o755:
    raise SystemExit(f'Guest root directory must be searchable by the unprivileged Agent (mode={root_mode!r})')
if tmp_mode is None or not stat.S_ISDIR(tmp_mode) or stat.S_IMODE(tmp_mode) != 0o1777:
    raise SystemExit(f'Guest /tmp must be writable by the unprivileged Agent (mode={tmp_mode!r})')
PY

GOOS=linux GOARCH="$other_arch" CGO_ENABLED=0 go build -trimpath -o "$test_dir/wrong-arch-init" ./cmd/hako-local-workspace-guest-init
if scripts/package-local-workspace-initrd.sh "$rootfs" "$test_dir/wrong-arch-init" "$test_dir/invalid.cpio.gz" >/dev/null 2>&1; then
  echo "initrd packager accepted a Guest init for another architecture" >&2
  exit 1
fi
# Kernel-module packaging must handle common OCI images where /lib points to a
# read-only /usr/lib tree. Built-in config avoids requiring fake module bytes.
module_rootfs="$test_dir/module-rootfs"
mkdir -p "$module_rootfs/usr/sbin" "$module_rootfs/usr/local/bin" \
  "$module_rootfs/opt/hako-code-server/bin" "$module_rootfs/etc/hako" \
  "$module_rootfs/usr/lib" "$test_dir/host-modules"
ln -s usr/lib "$module_rootfs/lib"
cp "$test_dir/guest-init" "$module_rootfs/usr/sbin/sshd"
cp "$test_dir/guest-init" "$module_rootfs/usr/local/bin/hako-workspace-agent"
cp "$test_dir/guest-init" "$module_rootfs/opt/hako-code-server/bin/code-server"
printf '{"keys":[]}\n' > "$module_rootfs/etc/hako/workspace-agent-public-keys.json"
chmod 0555 "$module_rootfs/usr/lib"
cat > "$test_dir/kernel.config" <<'EOF'
CONFIG_EXT4_FS=y
CONFIG_VIRTIO_MMIO=y
CONFIG_VIRTIO_BLK=y
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS_COMMON=y
CONFIG_VIRTIO_VSOCKETS=y
EOF
scripts/package-local-workspace-initrd.sh "$module_rootfs" "$test_dir/guest-init" \
  "$test_dir/workspace-initrd-modules.cpio.gz" "$test_dir/host-modules" "$test_dir/kernel.config"
if [ "$(stat -c '%a' "$module_rootfs/usr/lib")" != 555 ]; then
  echo "initrd packager did not restore the OCI /usr/lib mode" >&2
  exit 1
fi
gzip -dc "$test_dir/workspace-initrd-modules.cpio.gz" | cpio --quiet --list > "$test_dir/modules.list"
if ! grep -Fxq 'usr/lib/hako-local-workspace/modules' "$test_dir/modules.list"; then
  echo "initrd archive did not contain its local kernel module directory" >&2
  exit 1
fi
printf '%s\n' "local Workspace initrd packaging checks passed ($host_arch)"
