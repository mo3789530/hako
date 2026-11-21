"""Load a digest-pinned Hako source tar without extracting onto the host."""

import hashlib
import hmac
import io
import re
import tarfile

from .workspace import MAX_SOURCE_BYTES, MAX_SOURCE_FILES, MAX_SOURCE_FILE_BYTES, Workspace, validate_source_path

MAX_TAR_BYTES = 40 << 20
MAX_ENTRIES = 8192


def load_source(data: bytes, expected_sha256: str) -> Workspace:
    """Validate a complete source snapshot against its trusted exact-byte digest.

    It retains protected, binary and executable files without exposing protected
    files to model tools. Symlinks and special files remain unsupported.
    The caller must obtain the digest from authorized attempt metadata.
    """
    if (not isinstance(data, bytes) or not 1 <= len(data) <= MAX_TAR_BYTES or
            not isinstance(expected_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", expected_sha256)):
        raise ValueError("invalid source archive identity")
    if not hmac.compare_digest(hashlib.sha256(data).hexdigest(), expected_sha256):
        raise ValueError("source archive digest mismatch")
    files: dict[str, bytes] = {}
    executable: set[str] = set()
    seen: set[str] = set()
    total = 0
    try:
        # 'r:' accepts the uncompressed tar produced by workloadsource only.
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as archive:
            for member in archive:
                if len(seen) >= MAX_ENTRIES:
                    raise ValueError("too many source entries")
                name = member.name.removesuffix("/") if member.isdir() else member.name
                validate_source_path(name)
                if name in seen:
                    raise ValueError("duplicate source entry")
                seen.add(name)
                if member.isdir():
                    if member.size != 0:
                        raise ValueError("invalid source directory")
                    continue
                if (not member.isfile() or member.size < 0 or member.size > MAX_SOURCE_FILE_BYTES or
                        len(files) >= MAX_SOURCE_FILES):
                    raise ValueError("unsupported source file")
                total += member.size
                if total > MAX_SOURCE_BYTES:
                    raise ValueError("source size exceeds limit")
                source = archive.extractfile(member)
                if source is None:
                    raise ValueError("source file unavailable")
                with source:
                    content = source.read(MAX_SOURCE_FILE_BYTES + 1)
                if len(content) != member.size:
                    raise ValueError("truncated source file")
                files[name] = content
                if member.mode & 0o111:
                    executable.add(name)
    except (tarfile.TarError, OSError, EOFError) as exc:
        raise ValueError("invalid source archive") from exc
    # Reject explicit directories that collide with files even when empty.
    if any(name in files and any(other.startswith(name + "/") for other in seen) for name in files):
        raise ValueError("source file conflicts with a directory")
    return Workspace(files, executable=frozenset(executable))
