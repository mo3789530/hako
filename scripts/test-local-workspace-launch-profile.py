#!/usr/bin/env python3

import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("make-local-firecracker-launch-spec.py")


class LocalLaunchProfileTest(unittest.TestCase):
    def test_writes_private_digest_pinned_profile(self):
        with tempfile.TemporaryDirectory(prefix="hako-launch-profile-") as temporary:
            root = Path(temporary)
            kernel = root / "vmlinux"
            initrd = root / "workspace-initrd.cpio.gz"
            output = root / "launch.json"
            kernel.write_bytes(b"kernel bytes")
            initrd.write_bytes(b"initrd bytes")
            subprocess.run([sys.executable, str(SCRIPT), "--kernel", str(kernel), "--initrd", str(initrd), "--output", str(output)], check=True, capture_output=True, text=True)
            profile = json.loads(output.read_text())
            self.assertEqual(profile["kernel_image_sha256"], hashlib.sha256(b"kernel bytes").hexdigest())
            self.assertEqual(profile["initrd_sha256"], hashlib.sha256(b"initrd bytes").hexdigest())
            self.assertEqual(profile["kernel_boot_args"], "console=ttyS0 panic=1 reboot=k pci=off rdinit=/init")
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)

    def test_rejects_symlink_inputs_and_existing_output(self):
        with tempfile.TemporaryDirectory(prefix="hako-launch-profile-") as temporary:
            root = Path(temporary)
            real_kernel = root / "kernel.real"
            real_kernel.write_bytes(b"kernel")
            kernel = root / "kernel"
            kernel.symlink_to(real_kernel)
            initrd = root / "initrd"
            initrd.write_bytes(b"initrd")
            output = root / "launch.json"
            command = [sys.executable, str(SCRIPT), "--kernel", str(kernel), "--initrd", str(initrd), "--output", str(output)]
            rejected = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertFalse(output.exists())
            kernel.unlink()
            kernel.write_bytes(b"kernel")
            output.write_text("do not replace")
            rejected = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertEqual(output.read_text(), "do not replace")


if __name__ == "__main__":
    unittest.main()
