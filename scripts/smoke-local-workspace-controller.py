#!/usr/bin/env python3
"""Exercise the local Resource Controller API against a real Cloud Hypervisor Guest."""

from __future__ import annotations

import datetime
import hashlib
import json
import os
import pathlib
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


REPO = pathlib.Path(__file__).resolve().parents[1]
DEFAULT_KERNEL_ARGS = (
    "console=tty0 console=ttyS0 panic=1 reboot=k "
    "i8042.noaux i8042.nomux i8042.dumbkbd rdinit=/init"
)


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def request(socket_path: pathlib.Path, method: str, route: str, payload: dict | None = None) -> tuple[int, dict]:
    body = b"" if payload is None else json.dumps(payload, separators=(",", ":")).encode()
    headers = [f"{method} {route} HTTP/1.1", "Host: localhost", "Connection: close"]
    if payload is not None:
        headers.extend(("Content-Type: application/json", f"Content-Length: {len(body)}"))
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(180)
    connection.connect(str(socket_path))
    try:
        connection.sendall(("\r\n".join(headers) + "\r\n\r\n").encode() + body)
        chunks = []
        while True:
            chunk = connection.recv(65536)
            if not chunk:
                break
            chunks.append(chunk)
    finally:
        connection.close()
    response = b"".join(chunks)
    head, response_body = response.split(b"\r\n\r\n", 1)
    status = int(head.split(b"\r\n", 1)[0].split()[1])
    return status, json.loads(response_body)


def operation(revision: int, kind: str) -> dict:
    return {
        "schema_version": 2,
        "workspace_revision": revision,
        "operation_id": f"op_local_smoke_{revision}",
        "tenant_id": "tenant_local_smoke",
        "workspace_id": "workspace_local_smoke",
        "resource_plane_id": "rp_local_smoke",
        "type": kind,
        "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    }


def stop_smoke_cloud_hypervisor(state_dir: pathlib.Path, binary: pathlib.Path) -> bool:
    """Stop only a process whose private PID record and argv match this smoke."""
    clean = True
    for record_path in state_dir.glob("ch_*/cloud-hypervisor-process.json"):
        try:
            record = json.loads(record_path.read_text(encoding="utf-8"))
            pid = record["pid"]
            api_socket = record_path.parent / "api.sock"
            if type(pid) is not int or pid < 2:
                clean = False
                continue
            command = pathlib.Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            expected = [os.fsencode(binary), b"--api-socket", os.fsencode("path=" + str(api_socket))]
            if command[:3] != expected:
                clean = False
                continue
            os.kill(pid, signal.SIGTERM)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                if not pathlib.Path(f"/proc/{pid}").exists():
                    break
                time.sleep(0.05)
            if pathlib.Path(f"/proc/{pid}").exists():
                command = pathlib.Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
                if command[:3] == expected:
                    os.kill(pid, signal.SIGKILL)
                else:
                    clean = False
        except FileNotFoundError:
            continue
        except Exception:
            clean = False
    return clean


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: smoke-local-workspace-controller.py /path/to/cloud_hypervisor", file=sys.stderr)
        return 2
    cloud_hypervisor = pathlib.Path(sys.argv[1]).resolve(strict=True)
    kernel_default = pathlib.Path(f"/boot/vmlinuz-{os.uname().release}")
    kernel = pathlib.Path(os.environ.get("CLOUD_HYPERVISOR_KERNEL", str(kernel_default))).resolve(strict=True)
    initrd = pathlib.Path(os.environ.get("CLOUD_HYPERVISOR_INITRD", str(REPO / "build/local-workspace/workspace-initrd.cpio.gz"))).resolve(strict=True)
    controller = REPO / "build/hako-local-workspace-reconciler"
    if not controller.is_file() or not os.access("/dev/kvm", os.R_OK | os.W_OK):
        print("built local controller and readable/writable /dev/kvm are required", file=sys.stderr)
        return 2

    root = pathlib.Path(tempfile.mkdtemp(prefix="hako-local-controller-smoke-"))
    os.chmod(root, 0o700)
    state = root / "state"
    journal = root / "journal"
    state.mkdir(mode=0o700)
    journal.mkdir(mode=0o700)
    socket_path = root / "controller.sock"
    launch = {
        "kernel_image_path": str(kernel),
        "kernel_image_sha256": file_sha256(kernel),
        "initrd_path": str(initrd),
        "initrd_sha256": file_sha256(initrd),
        "kernel_boot_args": os.environ.get("CLOUD_HYPERVISOR_KERNEL_ARGS", DEFAULT_KERNEL_ARGS),
        "vcpu_count": 1,
        "memory_mib": 8192,
        "workspace_disk_size_mib": 128,
    }
    launch_path = root / "launch.json"
    launch_path.write_text(json.dumps(launch), encoding="utf-8")
    launch_path.chmod(0o600)
    log_path = root / "controller.log"
    env = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
    env.update(
        {
            "HAKO_LOCAL_CLOUD_HYPERVISOR_STATE_DIR": str(state),
            "HAKO_LOCAL_WORKSPACE_JOURNAL_DIR": str(journal),
            "HAKO_LOCAL_CLOUD_HYPERVISOR_BINARY": str(cloud_hypervisor),
            "HAKO_LOCAL_CLOUD_HYPERVISOR_LAUNCH_SPEC": str(launch_path),
            "HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET": str(socket_path),
            "HAKO_LOCAL_RESOURCE_PLANE_ID": "rp_local_smoke",
            "HAKO_LOCAL_WORKSPACE_GATEWAY_ORIGIN": "https://workspace-gateway.invalid",
            "HAKO_LOCAL_RECONCILE_INTERVAL": "10m",
        }
    )
    completed = False
    last_revision = 0
    process = None

    def start_controller(log_mode: str):
        nonlocal process
        with log_path.open(log_mode, encoding="utf-8") as log:
            process = subprocess.Popen(
                [str(controller)], cwd=REPO, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT
            )
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("local controller exited before becoming ready")
            if socket_path.exists():
                try:
                    if request(socket_path, "GET", "/healthz")[0] == 200:
                        return
                except OSError:
                    pass
            time.sleep(0.1)
        raise RuntimeError("local controller did not become ready")

    try:
        start_controller("w")
        expected = (
            ("ensure_running", "running"),
            ("suspend", "suspended"),
            ("resume", "running"),
            ("delete", "deleted"),
        )
        allocation_id = None
        last_command = None
        for last_revision, (kind, expected_state) in enumerate(expected, 1):
            last_command = operation(last_revision, kind)
            status, result = request(socket_path, "POST", "/v1/operations", last_command)
            if status != 200 or result.get("status") != "succeeded" or result.get("observed_state") != expected_state:
                raise RuntimeError(f"local {kind} operation did not reach {expected_state}")
            resource_id = result.get("external_resource_id")
            if kind == "ensure_running":
                if result.get("resource_kind") != "cloud_hypervisor_local" or not resource_id or result.get("allocation_generation") != 1:
                    raise RuntimeError("Ensure result omitted its local allocation binding")
                allocation_id = resource_id
            elif kind != "delete" and resource_id != allocation_id:
                raise RuntimeError("suspend/resume changed the persistent allocation ID")
            print(f"local Operation {kind}: {result['observed_state']}", flush=True)
            if kind == "resume":
                process.send_signal(signal.SIGTERM)
                process.wait(timeout=20)
                process = None
                deadline = time.monotonic() + 10
                while time.monotonic() < deadline and socket_path.exists():
                    time.sleep(0.05)
                if socket_path.exists():
                    raise RuntimeError("local controller did not release its private socket")
                start_controller("a")
                print("local controller restarted with the running allocation")
                retry_status, retry_result = request(socket_path, "POST", "/v1/operations", last_command)
                if retry_status != 200 or retry_result.get("status") != "succeeded" or retry_result.get("observed_state") != "running" or retry_result.get("external_resource_id") != allocation_id:
                    raise RuntimeError("local controller restart did not replay the same Resume result")
                print("local Resume retry after controller restart reused the allocation")
        if any(state.iterdir()):
            raise RuntimeError("delete left a Cloud Hypervisor allocation behind")
        completed = True
        print("LOCAL_CONTROLLER_CLOUD_HYPERVISOR_OPERATION_SMOKE_OK")
        return 0
    except Exception as error:
        print(f"local Workspace controller smoke failed: {error}", file=sys.stderr)
        if process is not None and process.poll() is None and socket_path.exists():
            try:
                status, result = request(socket_path, "POST", "/v1/operations", operation(last_revision + 1, "delete"))
                print(f"cleanup delete: HTTP {status}, {result.get('observed_state')}", file=sys.stderr)
            except Exception:
                pass
        return 1
    finally:
        if process is not None and process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        cloud_hypervisor_stopped = stop_smoke_cloud_hypervisor(state, cloud_hypervisor)
        if (completed or not any(state.glob("ch_*"))) and cloud_hypervisor_stopped:
            shutil.rmtree(root, ignore_errors=True)
        else:
            print(f"preserving local smoke state for inspection: {root}", file=sys.stderr)
        if completed and not cloud_hypervisor_stopped:
            raise RuntimeError("real Cloud Hypervisor process cleanup could not be confirmed")


if __name__ == "__main__":
    raise SystemExit(main())
