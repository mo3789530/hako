#!/bin/sh
set -eu

firecracker_bin=${FIRECRACKER_BIN:-firecracker}
timeout_seconds=${FIRECRACKER_SMOKE_TIMEOUT_SECONDS:-8}
case "$(uname -m)" in
  x86_64)
    guest_arch=amd64
		kernel_boot_args='console=tty0 console=ttyS0 panic=1 reboot=k pci=off i8042.noaux i8042.nomux i8042.dumbkbd rdinit=/init'
    default_kernel="/boot/vmlinuz-$(uname -r)"
    ;;
  aarch64|arm64)
    guest_arch=arm64
		kernel_boot_args='keep_bootcon console=tty0 console=ttyS0 panic=1 reboot=k pci=off rdinit=/init'
    default_kernel="/boot/Image-$(uname -r)"
    if [ ! -r "$default_kernel" ] && [ -r /boot/Image ]; then
      default_kernel=/boot/Image
    fi
    ;;
  *)
    echo "unsupported local Firecracker host architecture: $(uname -m)" >&2
    exit 1
    ;;
esac
kernel_image=${FIRECRACKER_KERNEL:-$default_kernel}
kernel_config=${FIRECRACKER_KERNEL_CONFIG:-/boot/config-$(uname -r)}

FIRECRACKER_BIN="$firecracker_bin" ./scripts/check-local-workspace-microvm-host.sh
if [ ! -r "$kernel_image" ]; then
  echo "readable host kernel image required: $kernel_image (set FIRECRACKER_KERNEL to override)" >&2
  exit 1
fi
if [ ! -r "$kernel_config" ] || ! grep -qx 'CONFIG_EXT4_FS=y' "$kernel_config"; then
  echo "Guest kernel config with built-in ext4 is required: $kernel_config (set FIRECRACKER_KERNEL_CONFIG to override)" >&2
  exit 1
fi
for tool in go python3 timeout mkfs.ext4 xz; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "required local Firecracker smoke tool not found: $tool" >&2
    exit 1
  fi
done
case "$timeout_seconds" in
  ''|*[!0-9]*) echo "FIRECRACKER_SMOKE_TIMEOUT_SECONDS must be a positive integer" >&2; exit 1 ;;
esac
if [ "$timeout_seconds" -lt 2 ] || [ "$timeout_seconds" -gt 60 ]; then
  echo "FIRECRACKER_SMOKE_TIMEOUT_SECONDS must be between 2 and 60" >&2
  exit 1
fi
for config_name in CONFIG_VSOCKETS CONFIG_VIRTIO_VSOCKETS CONFIG_VIRTIO_VSOCKETS_COMMON; do
	if ! grep -Eq "^${config_name}=(y|m)$" "$kernel_config"; then
		echo "Guest kernel must enable vsock support (${config_name}): $kernel_config" >&2
		exit 1
	fi
done

run_dir=$(mktemp -d "${TMPDIR:-/tmp}/hako-firecracker-smoke.XXXXXX")
cleanup() {
	if [ "${HAKO_KEEP_FIRECRACKER_SMOKE:-}" = 1 ]; then
		echo "Keeping Firecracker smoke artifacts at $run_dir" >&2
		return
	fi
  python3 -c 'import os,shutil,sys,tempfile; p=os.path.abspath(sys.argv[1]); parent=os.path.abspath(tempfile.gettempdir()); name=os.path.basename(p); shutil.rmtree(p) if os.path.dirname(p)==parent and name.startswith("hako-firecracker-smoke.") and os.path.isdir(p) and not os.path.islink(p) else None' "$run_dir"
}
trap cleanup EXIT HUP INT TERM

cat > "$run_dir/init.go" <<'EOF'
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "HAKO_FIRECRACKER_KVM_GUEST_INIT_FAILED: %v\n", err)
		os.Exit(1)
	}
	for { time.Sleep(time.Hour) }
}

func run() error {
	if module, err := os.Open("/virtio_mmio.ko"); err == nil {
		loadErr := unix.FinitModule(int(module.Fd()), "", 0)
		_ = module.Close()
		if loadErr != nil { return loadErr }
	} else if !errors.Is(err, os.ErrNotExist) { return err }
	if err := os.MkdirAll("/dev", 0755); err != nil { return err }
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"); err != nil { return err }
	if err := os.MkdirAll("/proc", 0755); err != nil { return err }
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil { return err }
	mode := ""
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil { return err }
	for _, field := range strings.Fields(string(cmdline)) {
		if strings.HasPrefix(field, "hako_disk_mode=") { mode = strings.TrimPrefix(field, "hako_disk_mode=") }
	}
	if err := os.MkdirAll("/workspace", 0755); err != nil { return err }
	if err := syscall.Mount("/dev/vda", "/workspace", "ext4", syscall.MS_NODEV|syscall.MS_NOSUID, ""); err != nil { return err }
	marker := []byte("HAKO_WORKSPACE_FILESYSTEM_PERSISTED_V1")
	switch mode {
	case "write":
		if read, err := os.ReadFile("/workspace/hako-persistence-marker"); err == nil {
			if string(read) != string(marker) { return errors.New("persistent disk marker differs") }
			fmt.Fprintln(os.Stdout, "HAKO_LOCAL_DRIVER_DISK_RESTORED_OK")
		} else if errors.Is(err, os.ErrNotExist) {
			file, err := os.OpenFile("/workspace/hako-persistence-marker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil { return err }
			if _, err := file.Write(marker); err != nil { _ = file.Close(); return err }
			if err := file.Sync(); err != nil { _ = file.Close(); return err }
			if err := file.Close(); err != nil { return err }
			fmt.Fprintln(os.Stdout, "HAKO_FIRECRACKER_FILESYSTEM_WRITE_OK")
		} else { return err }
	case "verify":
		read, err := os.ReadFile("/workspace/hako-persistence-marker")
		if err != nil { return err }
		if string(read) != string(marker) { return errors.New("persistent disk marker differs") }
		fmt.Fprintln(os.Stdout, "HAKO_FIRECRACKER_FILESYSTEM_RESUME_OK")
	default:
		return errors.New("unknown disk smoke mode")
	}
	fmt.Fprintln(os.Stdout, "HAKO_LOCAL_GUEST_READY_V1")
	if strings.Contains(string(cmdline), "hako_enable_vsock=1") {
		if err := startVsockAgent(); err != nil { return err }
		fmt.Fprintln(os.Stdout, "HAKO_LOCAL_GUEST_VSOCK_READY_V1")
	}
	return nil
}

func startVsockAgent() error {
	for _, module := range []string{"/vsock.ko", "/vmw_vsock_virtio_transport_common.ko", "/vmw_vsock_virtio_transport.ko"} {
		file, err := os.Open(module)
		if err == nil {
			loadErr := unix.FinitModule(int(file.Fd()), "", 0)
			_ = file.Close()
			if loadErr != nil && loadErr != unix.EEXIST { return loadErr }
		} else if !errors.Is(err, os.ErrNotExist) { return err }
	}
	server := exec.Command("/vsock-smoke-agent")
	server.Stdout, server.Stderr = os.Stdout, os.Stderr
	if err := server.Start(); err != nil { return err }
	return nil
}
EOF
CGO_ENABLED=0 GOOS=linux GOARCH="$guest_arch" go build -o "$run_dir/init" "$run_dir/init.go"
CGO_ENABLED=0 GOOS=linux GOARCH="$guest_arch" go build -trimpath -ldflags="-s -w" \
  -o "$run_dir/vsock-smoke-agent" ./cmd/hako-workspace-vsock-smoke-guest
disk_image="$run_dir/workspace-disk.raw"
truncate -s 64M "$disk_image"
mkfs.ext4 -q -F -L HAKO_WORKSPACE "$disk_image"
module_image="$run_dir/virtio_mmio.ko"
module_source=${FIRECRACKER_VIRTIO_MMIO_MODULE:-}
if [ -z "$module_source" ] && [ "$kernel_image" = "$default_kernel" ]; then
  module_source="$(find "/lib/modules/$(uname -r)" -type f -name 'virtio_mmio.ko*' -print -quit 2>/dev/null || true)"
fi
if [ -n "$module_source" ]; then
  case "$module_source" in
    *.xz) xz -dc "$module_source" > "$module_image" ;;
    *.zst) zstd -dc "$module_source" > "$module_image" ;;
    *) cp "$module_source" "$module_image" ;;
  esac
fi
vsock_module_root=${FIRECRACKER_VSOCK_MODULE_DIR:-/lib/modules/$(uname -r)/kernel/net/vmw_vsock}
prepare_vsock_module() {
  name=$1
  config_name=$2
  destination="$run_dir/$name.ko"
  source=$(find "$vsock_module_root" -maxdepth 1 -type f -name "$name.ko*" -print -quit 2>/dev/null || true)
  if [ -z "$source" ]; then
    if grep -qx "${config_name}=m" "$kernel_config"; then
      echo "matching Guest vsock module missing: $name.ko in $vsock_module_root" >&2
      exit 1
    fi
    return
  fi
  case "$source" in
    *.xz) xz -dc "$source" > "$destination" ;;
    *.zst) zstd -dc "$source" > "$destination" ;;
    *) cp "$source" "$destination" ;;
  esac
}
prepare_vsock_module vsock CONFIG_VSOCKETS
prepare_vsock_module vmw_vsock_virtio_transport_common CONFIG_VIRTIO_VSOCKETS_COMMON
prepare_vsock_module vmw_vsock_virtio_transport CONFIG_VIRTIO_VSOCKETS
python3 - "$run_dir" "$kernel_image" "$kernel_boot_args" "$disk_image" "$module_image" "$run_dir/vsock-smoke-agent" \
  "$run_dir/vsock.ko" "$run_dir/vmw_vsock_virtio_transport_common.ko" "$run_dir/vmw_vsock_virtio_transport.ko" <<'PY'
import json, os, stat, sys, time

root, kernel, boot_args, disk, module, vsock_agent = sys.argv[1:7]
vsock_modules = sys.argv[7:]
archive = os.path.join(root, 'initrd.cpio')
with open(archive, 'wb') as output:
    def entry(name, mode, data=b'', inode=1):
        fields = (inode, mode, 0, 0, 1, int(time.time()), len(data), 0, 0, 0, 0, len(name) + 1, 0)
        output.write(b'070701' + b''.join(f'{field:08x}'.encode() for field in fields))
        output.write(name.encode() + b'\0')
        output.write(b'\0' * (-output.tell() % 4))
        output.write(data)
        output.write(b'\0' * (-output.tell() % 4))

    with open(os.path.join(root, 'init'), 'rb') as init:
        entry('init', stat.S_IFREG | 0o755, init.read())
    with open(vsock_agent, 'rb') as agent:
        entry('vsock-smoke-agent', stat.S_IFREG | 0o755, agent.read(), 2)
    if os.path.isfile(module):
        with open(module, 'rb') as module_file:
            entry('virtio_mmio.ko', stat.S_IFREG | 0o600, module_file.read(), 3)
    for index, module_path in enumerate(vsock_modules, 4):
        if os.path.isfile(module_path):
            with open(module_path, 'rb') as module_file:
                entry(os.path.basename(module_path), stat.S_IFREG | 0o600, module_file.read(), index)
    entry('TRAILER!!!', 0, b'', 2)

config = {
    'boot-source': {
        'kernel_image_path': kernel,
        'initrd_path': archive,
        'boot_args': boot_args,
    },
    'drives': [{'drive_id': 'workspace', 'path_on_host': disk,
                'is_root_device': False, 'is_read_only': False}],
    'machine-config': {'vcpu_count': 1, 'mem_size_mib': 256, 'smt': False},
}
with open(os.path.join(root, 'config.json'), 'w') as output:
    json.dump(config, output)
PY

# Exercise the concrete Resource Controller driver against a disposable Guest,
# including clean guest quiescence, VMM stop, driver reconstruction, cold boot
# from the same disk, persistence verification, and cleanup.
go run ./scripts/local_firecracker_driver_smoke.go \
  "$firecracker_bin" "$kernel_image" "$run_dir/initrd.cpio" \
  "$run_dir/driver-state" "$kernel_boot_args"

run_guest() {
  mode=$1
  expected=$2
  python3 - "$run_dir" "$kernel_boot_args" "$mode" <<'PY'
import json, os, sys
root, boot_args, mode = sys.argv[1:]
config_path = os.path.join(root, 'config.json')
with open(config_path) as source:
    config = json.load(source)
config['boot-source']['boot_args'] = boot_args + ' hako_disk_mode=' + mode
with open(config_path, 'w') as output:
    json.dump(config, output)
PY
  status=0
  timeout "$timeout_seconds" "$firecracker_bin" \
    --id "hako-local-smoke-$$-$mode" \
    --api-sock "$run_dir/api-$mode.sock" \
    --config-file "$run_dir/config.json" > "$run_dir/firecracker-$mode.log" 2>&1 || status=$?
  if [ "$status" -ne 124 ] && [ "$status" -ne 0 ]; then
    cat "$run_dir/firecracker-$mode.log" >&2
    exit "$status"
  fi
  if ! grep -F "$expected" "$run_dir/firecracker-$mode.log"; then
    cat "$run_dir/firecracker-$mode.log" >&2
    echo "Firecracker did not complete the ${mode} Guest disk check within ${timeout_seconds}s" >&2
    exit 1
  fi
}

run_guest write HAKO_FIRECRACKER_FILESYSTEM_WRITE_OK
run_guest verify HAKO_FIRECRACKER_FILESYSTEM_RESUME_OK
echo "Firecracker KVM boot, ext4 /workspace mount, and persistent file restart passed (kernel: $kernel_image; no Hako Workspace image was used)."
