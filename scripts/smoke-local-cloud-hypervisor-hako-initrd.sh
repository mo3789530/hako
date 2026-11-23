#!/bin/sh
set -eu

cloud_hypervisor_bin=${CLOUD_HYPERVISOR_BIN:-cloud-hypervisor}
initrd=${CLOUD_HYPERVISOR_INITRD:-build/local-workspace/workspace-initrd.cpio.gz}
case "$(uname -m)" in
  x86_64)
    kernel_args='console=tty0 console=ttyS0 panic=1 reboot=k i8042.noaux i8042.nomux i8042.dumbkbd rdinit=/init'
    default_kernel="/boot/vmlinuz-$(uname -r)"
    ;;
  aarch64|arm64)
    kernel_args='keep_bootcon console=tty0 console=ttyS0 panic=1 reboot=k rdinit=/init'
    default_kernel="/boot/Image-$(uname -r)"
    [ -r "$default_kernel" ] || default_kernel=/boot/Image
    ;;
  *) echo "unsupported local Cloud Hypervisor host architecture: $(uname -m)" >&2; exit 1 ;;
esac
kernel=${CLOUD_HYPERVISOR_KERNEL:-$default_kernel}
kernel_config=${CLOUD_HYPERVISOR_KERNEL_CONFIG:-/boot/config-$(uname -r)}

CLOUD_HYPERVISOR_BIN="$cloud_hypervisor_bin" ./scripts/check-local-cloud-hypervisor-host.sh
cloud_hypervisor_bin=$(command -v "$cloud_hypervisor_bin")
if [ ! -r "$kernel" ] || [ ! -r "$kernel_config" ] || ! grep -qx 'CONFIG_EXT4_FS=y' "$kernel_config"; then
  echo "readable Guest kernel and config with built-in ext4 are required" >&2
  exit 1
fi
if [ ! -f "$initrd" ] || [ -L "$initrd" ]; then
  echo "Hako Workspace initrd not found: $initrd (run make build-local-workspace-native-rootfs)" >&2
  exit 1
fi
for tool in go python3 realpath; do
  command -v "$tool" >/dev/null 2>&1 || { echo "required smoke tool not found: $tool" >&2; exit 1; }
done

kernel=$(realpath "$kernel")
initrd=$(realpath "$initrd")
state_dir=$(mktemp -d "${TMPDIR:-/tmp}/hako-cloud-hypervisor-workspace.XXXXXX")
cleanup() {
  if [ "${HAKO_LOCAL_WORKSPACE_SMOKE_KEEP_STATE:-}" = true ]; then
    printf 'preserved local Workspace smoke state: %s\n' "$state_dir"
    return
  fi
  python3 -c 'import os,shutil,sys,tempfile; p=os.path.abspath(sys.argv[1]); parent=os.path.abspath(tempfile.gettempdir()); name=os.path.basename(p); shutil.rmtree(p) if os.path.dirname(p)==parent and name.startswith("hako-cloud-hypervisor-workspace.") and os.path.isdir(p) and not os.path.islink(p) else None' "$state_dir"
}
trap cleanup EXIT HUP INT TERM
go run -tags cloud_hypervisor_workspace_smoke ./scripts/local_cloud_hypervisor_workspace_smoke.go \
  "$cloud_hypervisor_bin" "$kernel" "$initrd" "$state_dir" "$kernel_args"
