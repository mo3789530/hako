"""Create bounded, deterministic proposals for Hako's independent verifier.

Inputs are captured file contents, not paths to read from the filesystem. The
caller is responsible for safely capturing changes against the pinned base.
The receiver must independently validate the archive and its provenance.
"""

import hashlib
import io
import stat
import zipfile
from collections.abc import Mapping
from dataclasses import dataclass

MAX_FILES = 128
MAX_FILE_BYTES = 128 << 10
MAX_ARCHIVE_BYTES = 8 << 20
_PROTECTED = {".git", ".hako", ".github", ".aws", ".ssh", "terraform", "infra"}
_MARKERS = (
    b"-----begin private key-----",
    b"-----begin rsa private key-----",
    b"-----begin ec private key-----",
    b"-----begin openssh private key-----",
    b"sk-ant-api",
    b"akia",
)


@dataclass(frozen=True)
class Proposal:
    data: bytes
    sha256: str


def _validate_path(path: str) -> None:
    if not isinstance(path, str):
        raise ValueError("invalid proposal path")
    try:
        length = len(path.encode("utf-8"))
    except UnicodeEncodeError as exc:
        raise ValueError("invalid proposal path") from exc
    parts = path.split("/")
    if (
        not 1 <= length <= 512
        or any(c in path for c in "\\\r\n\0:")
        or any(part in ("", ".", "..") or part.lower() in _PROTECTED for part in parts)
    ):
        raise ValueError("invalid proposal path")
    base = parts[-1].lower()
    # Terraform has no enabled verifier profile yet. Deny its source and state
    # formats anywhere in the tree, not only under conventional directory names.
    if (
        base.endswith((".tf", ".tfvars", ".tfstate", ".tfstate.backup"))
        or base == ".terraform.lock.hcl"
        or base.endswith(".tf.json")
    ):
        raise ValueError("Terraform proposals are not enabled")
    if (
        base.startswith(".env")
        or base in {"credentials", "id_rsa", "id_ed25519"}
        or base.endswith((".pem", ".key", ".p12", ".pfx"))
    ):
        raise ValueError("protected proposal path")


def create_proposal(files: Mapping[str, bytes]) -> Proposal:
    """Encode regular UTF-8 files; deletions and mode changes are unsupported."""
    if not 1 <= len(files) <= MAX_FILES:
        raise ValueError("invalid proposal file count")
    entries = []
    total = 0
    for path, content in files.items():
        _validate_path(path)
        if not isinstance(content, bytes) or len(content) > MAX_FILE_BYTES or b"\0" in content:
            raise ValueError("invalid proposal content")
        try:
            content.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("invalid proposal content") from exc
        if any(marker in content.lower() for marker in _MARKERS):
            raise ValueError("proposal contains a credential marker")
        total += len(content)
        if total > MAX_ARCHIVE_BYTES:
            raise ValueError("proposal too large")
        entries.append((path, content))
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for path, content in sorted(entries):
            info = zipfile.ZipInfo(path, (1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(info, content)
    data = buffer.getvalue()
    if len(data) > MAX_ARCHIVE_BYTES:
        raise ValueError("proposal archive too large")
    return Proposal(data, hashlib.sha256(data).hexdigest())
