#!/usr/bin/env python3
"""Write a private Cloud Hypervisor base launch profile pinned to kernel/initrd bytes."""

import argparse
import hashlib
import json
import os
import re
import stat
from pathlib import Path


def digest(path: Path) -> str:
    metadata = path.lstat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_size <= 0 or metadata.st_size > 16 << 30:
        raise ValueError("kernel/initrd must be a bounded regular non-symlink file")
    checksum = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            checksum.update(block)
    return checksum.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--kernel", required=True, type=Path)
    parser.add_argument("--initrd", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--memory-mib", type=int, default=8192)
    parser.add_argument("--workspace-disk-mib", type=int, default=10240)
    parser.add_argument("--vcpus", type=int, default=2)
    args = parser.parse_args()

    for path in (args.kernel, args.initrd):
        if not path.is_absolute() or path.resolve(strict=True) != path:
            raise ValueError("kernel and initrd paths must be clean absolute paths")
    if not args.output.is_absolute() or args.output.resolve() != args.output:
        raise ValueError("output path must be a clean absolute path")
    if not 1 <= args.vcpus <= 8 or not 2048 <= args.memory_mib <= 32768 or not 128 <= args.workspace_disk_mib <= 102400:
        raise ValueError("vCPU, memory, or Workspace disk size is outside its allowed bounds")
    boot_args = "console=ttyS0 panic=1 reboot=k rdinit=/init"
    if re.search(r"(?:^|\s)hako\.[^\s]+", boot_args):
        raise ValueError("base kernel profile must not pin Workspace identity")
    payload = {
        "kernel_image_path": str(args.kernel),
        "kernel_image_sha256": digest(args.kernel),
        "initrd_path": str(args.initrd),
        "initrd_sha256": digest(args.initrd),
        "kernel_boot_args": boot_args,
        "vcpu_count": args.vcpus,
        "memory_mib": args.memory_mib,
        "workspace_disk_size_mib": args.workspace_disk_mib,
    }
    encoded = (json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n").encode()
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(args.output, flags, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(encoded)
            output.flush()
            os.fsync(output.fileno())
    except Exception:
        try:
            os.unlink(args.output)
        except OSError:
            pass
        raise
    print(f"wrote private Cloud Hypervisor profile {args.output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
