#!/bin/sh
set -eu

rootfs=${1:?usage: package-local-workspace-initrd.sh ROOTFS_DIR GUEST_INIT_BIN OUTPUT}
guest_init=${2:?usage: package-local-workspace-initrd.sh ROOTFS_DIR GUEST_INIT_BIN OUTPUT}
output=${3:?usage: package-local-workspace-initrd.sh ROOTFS_DIR GUEST_INIT_BIN OUTPUT}
host_module_dir=${4:-}
host_kernel_config=${5:-}

case "$rootfs" in /*) ;; *) echo "rootfs directory must be an absolute path" >&2; exit 1 ;; esac
case "$guest_init" in /*) ;; *) guest_init="$(pwd)/$guest_init" ;; esac
case "$output" in /*) ;; *) output="$(pwd)/$output" ;; esac
if [ ! -d "$rootfs" ] || [ -L "$rootfs" ] || [ ! -x "$guest_init" ] || [ -L "$guest_init" ]; then
  echo "rootfs must be a real directory and Guest init must be a regular executable" >&2
  exit 1
fi
for path in usr/sbin/sshd usr/local/bin/hako-workspace-agent opt/hako-code-server/bin/code-server etc/hako/workspace-agent-public-keys.json; do
  if [ ! -f "$rootfs/$path" ] || [ -L "$rootfs/$path" ]; then
    echo "Workspace rootfs is missing required Guest component: /$path" >&2
    exit 1
  fi
done
for tool in python3 cpio gzip cp find mktemp mv install rm; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "required initrd packaging tool not found: $tool" >&2
    exit 1
  fi
done

python3 - "$rootfs/usr/sbin/sshd" "$guest_init" <<'PY'
import os, struct, sys
def elf(path):
    path = os.path.realpath(path)
    with open(path, 'rb') as f:
        header = f.read(64)
        if len(header) < 52 or header[:4] != b'\x7fELF' or header[4] not in (1, 2) or header[5] not in (1, 2):
            raise SystemExit(f'not a supported ELF executable: {path}')
        endian = '<' if header[5] == 1 else '>'
        machine = struct.unpack(endian + 'H', header[18:20])[0]
        if header[4] == 2:
            phoff = struct.unpack(endian + 'Q', header[32:40])[0]
            phentsize, phnum = struct.unpack(endian + 'HH', header[54:58])
        else:
            phoff = struct.unpack(endian + 'I', header[28:32])[0]
            phentsize, phnum = struct.unpack(endian + 'HH', header[42:46])
        if phentsize < 4 or phnum > 1024:
            raise SystemExit(f'invalid ELF program headers: {path}')
        dynamic_loader = False
        for index in range(phnum):
            f.seek(phoff + index * phentsize)
            entry = f.read(4)
            if len(entry) != 4:
                raise SystemExit(f'truncated ELF program headers: {path}')
            if struct.unpack(endian + 'I', entry)[0] == 3:
                dynamic_loader = True
        return machine, dynamic_loader

host = os.uname().machine
expected = {'x86_64': 62, 'aarch64': 183, 'arm64': 183}.get(host)
if expected is None:
    raise SystemExit(f'unsupported host architecture: {host}')
sshd_machine, _ = elf(sys.argv[1])
if sshd_machine != expected:
    raise SystemExit(f'Workspace rootfs architecture does not match host {host}')
init_machine, dynamic_loader = elf(sys.argv[2])
if init_machine != expected or dynamic_loader:
    raise SystemExit('Guest PID 1 must be a static executable matching the host architecture')
PY

output_parent=$(dirname "$output")
mkdir -p "$output_parent"
stage=$(mktemp -d "${TMPDIR:-/tmp}/hako-local-initrd.XXXXXX")
archive_tmp=$(mktemp "$output_parent/.workspace-initrd.XXXXXX")
cleanup() {
  rm -f "$archive_tmp"
  if [ -d "$stage" ]; then
    # Rootfs images commonly contain read-only directories. They are correct
    # archive permissions but would prevent an unprivileged build user from
    # removing its own temporary staging tree.
    find "$stage" -type d -exec chmod u+rwx {} +
    rm -rf "$stage"
  fi
}
trap cleanup EXIT HUP INT TERM
cp -a "$rootfs/." "$stage/"
# mktemp creates a private 0700 directory. That is useful for constructing the
# archive, but it becomes the Guest's root directory in newc; non-root Guest
# processes then cannot traverse any path even when the files are executable.
chmod 0755 "$stage"
mkdir -p "$stage/dev" "$stage/proc" "$stage/sys" "$stage/tmp" "$stage/workspace"
chmod 1777 "$stage/tmp"
if [ -n "$host_module_dir" ] || [ -n "$host_kernel_config" ]; then
  if [ ! -d "$host_module_dir" ] || [ -L "$host_module_dir" ] || [ ! -f "$host_kernel_config" ] || [ -L "$host_kernel_config" ]; then
    echo "host kernel module directory and kernel config must both be supplied" >&2
    exit 1
  fi
  python3 - "$host_module_dir" "$host_kernel_config" "$stage" <<'PY'
import glob, os, re, stat, subprocess, sys

module_root, config_path, stage = sys.argv[1:]
config = {}
with open(config_path, encoding='ascii') as source:
    for line in source:
        match = re.fullmatch(r'(CONFIG_[A-Z0-9_]+)=(y|m|n)\n?', line)
        if match:
            config[match.group(1)] = match.group(2)
required = {
    'CONFIG_EXT4_FS': 'y',
    'CONFIG_VIRTIO_MMIO': None,
    'CONFIG_VIRTIO_BLK': None,
    'CONFIG_VSOCKETS': None,
    'CONFIG_VIRTIO_VSOCKETS_COMMON': None,
    'CONFIG_VIRTIO_VSOCKETS': None,
}
for name, expected in required.items():
    value = config.get(name, 'n')
    if value not in ('y', 'm') or expected == 'y' and value != expected:
        raise SystemExit(f'Guest kernel config has unsupported {name}={value}')
module_names = {
    'CONFIG_VIRTIO_MMIO': 'virtio_mmio',
    'CONFIG_VIRTIO_BLK': 'virtio_blk',
    'CONFIG_VSOCKETS': 'vsock',
    'CONFIG_VIRTIO_VSOCKETS_COMMON': 'vmw_vsock_virtio_transport_common',
    'CONFIG_VIRTIO_VSOCKETS': 'vmw_vsock_virtio_transport',
}
destination = os.path.join(stage, 'lib', 'hako-local-workspace', 'modules')
stage_root = os.path.realpath(stage)
library_root = os.path.realpath(os.path.join(stage, 'lib'))
if os.path.commonpath((stage_root, library_root)) != stage_root:
    raise SystemExit('Guest kernel module directory escapes the initrd staging root')
library_mode = stat.S_IMODE(os.stat(library_root).st_mode)
try:
    # OCI images often make /usr/lib read-only and expose /lib as a symlink.
    # Temporarily make only the in-stage library root writable to add our
    # fixed module directory, then preserve the image's original mode.
    os.chmod(library_root, library_mode | 0o700)
    os.makedirs(destination, mode=0o755, exist_ok=True)
finally:
    os.chmod(library_root, library_mode)
manifest = []
for option, name in module_names.items():
    if config[option] != 'm':
        continue
    candidates = sorted(glob.glob(os.path.join(module_root, '**', name + '.ko*'), recursive=True))
    if len(candidates) != 1 or not os.path.isfile(candidates[0]) or os.path.islink(candidates[0]):
        raise SystemExit(f'Guest kernel module is missing or ambiguous: {name}')
    source = candidates[0]
    if source.endswith('.xz'):
        content = subprocess.run(['xz', '-dc', source], check=True, stdout=subprocess.PIPE).stdout
    elif source.endswith('.zst'):
        content = subprocess.run(['zstd', '-dc', source], check=True, stdout=subprocess.PIPE).stdout
    elif source.endswith('.ko'):
        with open(source, 'rb') as module:
            content = module.read()
    else:
        raise SystemExit(f'unsupported compressed kernel module: {name}')
    with open(os.path.join(destination, name + '.ko'), 'xb') as module:
        module.write(content)
    os.chmod(os.path.join(destination, name + '.ko'), 0o600)
    manifest.append(name + '.ko')
with open(os.path.join(stage, 'etc', 'hako', 'local-workspace-kernel-modules'), 'w', encoding='ascii') as output:
    output.write(''.join(name + '\n' for name in manifest))
os.chmod(os.path.join(stage, 'etc', 'hako', 'local-workspace-kernel-modules'), 0o444)
PY
fi
if [ -e "$stage/init" ] || [ -L "$stage/init" ]; then
  rm -f "$stage/init"
fi
install -m 0755 "$guest_init" "$stage/init"
(
  cd "$stage"
  find . -xdev -print0 | cpio --null --quiet --create --format=newc --owner=0:0
) | gzip -n > "$archive_tmp"
test -s "$archive_tmp"
mv -f "$archive_tmp" "$output"
cleanup
trap - EXIT HUP INT TERM
printf 'built host-architecture Cloud Hypervisor initramfs: %s\n' "$output"
