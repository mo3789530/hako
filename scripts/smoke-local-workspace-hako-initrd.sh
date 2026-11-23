#!/bin/sh
set -eu

firecracker_bin=${FIRECRACKER_BIN:-firecracker}
initrd=${FIRECRACKER_INITRD:-build/local-workspace/workspace-initrd.cpio.gz}
case "$(uname -m)" in
  x86_64)
    kernel_args='console=tty0 console=ttyS0 panic=1 reboot=k pci=off i8042.noaux i8042.nomux i8042.dumbkbd rdinit=/init'
    default_kernel="/boot/vmlinuz-$(uname -r)"
    ;;
  aarch64|arm64)
    kernel_args='keep_bootcon console=tty0 console=ttyS0 panic=1 reboot=k pci=off rdinit=/init'
    default_kernel="/boot/Image-$(uname -r)"
    [ -r "$default_kernel" ] || default_kernel=/boot/Image
    ;;
  *) echo "unsupported local Firecracker host architecture: $(uname -m)" >&2; exit 1 ;;
esac
kernel=${FIRECRACKER_KERNEL:-$default_kernel}
kernel_config=${FIRECRACKER_KERNEL_CONFIG:-/boot/config-$(uname -r)}
module_dir=${FIRECRACKER_VSOCK_MODULE_DIR:-/lib/modules/$(uname -r)}

FIRECRACKER_BIN="$firecracker_bin" ./scripts/check-local-workspace-microvm-host.sh
firecracker_bin=$(command -v "$firecracker_bin")
if [ ! -r "$kernel" ]; then
  echo "readable host kernel image required: $kernel (set FIRECRACKER_KERNEL to override)" >&2
  exit 1
fi
if [ ! -r "$kernel_config" ] || ! grep -qx 'CONFIG_EXT4_FS=y' "$kernel_config"; then
  echo "Guest kernel config with built-in ext4 is required: $kernel_config" >&2
  exit 1
fi
for option in CONFIG_VIRTIO_MMIO CONFIG_VIRTIO_BLK CONFIG_VSOCKETS CONFIG_VIRTIO_VSOCKETS_COMMON CONFIG_VIRTIO_VSOCKETS; do
  if ! grep -Eq "^${option}=(y|m)$" "$kernel_config"; then
    echo "Guest kernel must enable ${option}: $kernel_config" >&2
    exit 1
  fi
done
if [ ! -f "$initrd" ] || [ -L "$initrd" ]; then
  echo "Hako Workspace initrd not found: $initrd (run make build-local-workspace-native-rootfs)" >&2
  exit 1
fi
kernel=$(realpath "$kernel")
initrd=$(realpath "$initrd")
for tool in go python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "required smoke tool not found: $tool" >&2; exit 1; }
done

state_dir=$(mktemp -d "${TMPDIR:-/tmp}/hako-firecracker-workspace.XXXXXX")
cleanup() {
  if [ "${HAKO_LOCAL_WORKSPACE_SMOKE_KEEP_STATE:-}" = true ]; then
    printf 'preserved local Workspace smoke state: %s\n' "$state_dir"
    return
  fi
  python3 -c 'import os,shutil,sys,tempfile; p=os.path.abspath(sys.argv[1]); parent=os.path.abspath(tempfile.gettempdir()); name=os.path.basename(p); shutil.rmtree(p) if os.path.dirname(p)==parent and name.startswith("hako-firecracker-workspace.") and os.path.isdir(p) and not os.path.islink(p) else None' "$state_dir"
}
trap cleanup EXIT HUP INT TERM
FIRECRACKER_VSOCK_MODULE_DIR="$module_dir" go run ./scripts/local_firecracker_workspace_smoke.go \
  "$firecracker_bin" "$kernel" "$initrd" "$state_dir" "$kernel_args"
