"""Bounded in-memory workspace for the coding agent's file tools.

The initial contents must come from a trusted, separately verified source
snapshot. This module deliberately does not read paths from the host filesystem.
"""

import hashlib
from collections.abc import Mapping

from .proposal import MAX_FILE_BYTES, MAX_FILES, Proposal, _validate_path, create_proposal

MAX_SOURCE_FILES = 4096
MAX_SOURCE_BYTES = 32 << 20
MAX_CHANGED_BYTES = 8 << 20
MAX_SOURCE_FILE_BYTES = 8 << 20


def validate_source_path(path: str) -> None:
    if not isinstance(path, str):
        raise ValueError("invalid source path")
    try:
        size = len(path.encode("utf-8"))
    except UnicodeEncodeError as exc:
        raise ValueError("invalid source path") from exc
    if (not 1 <= size <= 4096 or any(c in path for c in "\\\r\n\0") or
            any(part in ("", ".", "..") for part in path.split("/"))):
        raise ValueError("invalid source path")


def _visible_path(path: str) -> bool:
    try:
        _validate_path(path)
        return True
    except ValueError:
        return False


class Workspace:
    """An isolated snapshot with bounded read, list and write operations.

    Source files are kept in full; protected paths are hidden from model tools.
    Executable files are readable when text but cannot be changed because the
    current proposal format does not preserve executable permissions.
    """

    def __init__(self, files: Mapping[str, bytes], *, executable: frozenset[str] = frozenset()):
        if len(files) > MAX_SOURCE_FILES:
            raise ValueError("source file count exceeds limit")
        snapshot: dict[str, bytes] = {}
        total = 0
        for path, content in files.items():
            validate_source_path(path)
            self._validate_source(content)
            total += len(content)
            if total > MAX_SOURCE_BYTES:
                raise ValueError("source size exceeds limit")
            snapshot[path] = content
        for path in snapshot:
            self._validate_shape(path, snapshot)
        if not isinstance(executable, frozenset) or not executable.issubset(snapshot):
            raise ValueError("invalid executable source paths")
        self._executable = executable
        self._base = snapshot.copy()
        self._files = snapshot

    @staticmethod
    def _validate_source(content: bytes) -> None:
        if not isinstance(content, bytes) or len(content) > MAX_SOURCE_FILE_BYTES:
            raise ValueError("invalid source content")

    def list_files(self) -> tuple[str, ...]:
        return tuple(sorted(path for path in self._files if _visible_path(path)))

    def list_changes(self) -> tuple[str, ...]:
        return tuple(sorted(self._changes()))

    def read_file(self, path: str) -> bytes:
        _validate_path(path)
        return self._read_text(self._files[path])

    def read_base_file(self, path: str) -> bytes:
        _validate_path(path)
        return self._read_text(self._base[path])

    @staticmethod
    def _read_text(content: bytes) -> bytes:
        if len(content) > MAX_FILE_BYTES or b"\0" in content:
            raise ValueError("file is not supported by the text tool")
        try:
            content.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise ValueError("file is not supported by the text tool") from exc
        return content

    def write_file(self, path: str, content: bytes) -> None:
        # Reuse the proposal validator for protected paths and credential markers.
        create_proposal({path: content})
        if path in self._executable:
            raise ValueError("executable source changes are not supported")
        self._validate_shape(path, self._files)
        changed = self._changes()
        if path in self._base and self._base[path] == content:
            changed.pop(path, None)
        else:
            changed[path] = content
        if len(changed) > MAX_FILES or sum(map(len, changed.values())) > MAX_CHANGED_BYTES:
            raise ValueError("change limit exceeded")
        if path not in self._files and len(self._files) >= MAX_SOURCE_FILES:
            raise ValueError("source file count exceeds limit")
        current_size = sum(map(len, self._files.values()))
        if current_size - len(self._files.get(path, b"")) + len(content) > MAX_SOURCE_BYTES:
            raise ValueError("source size exceeds limit")
        self._files[path] = content

    @staticmethod
    def _validate_shape(path: str, files: Mapping[str, bytes]) -> None:
        parts = path.split("/")
        if any("/".join(parts[:i]) in files for i in range(1, len(parts))):
            raise ValueError("source file conflicts with a directory")
        if any(existing.startswith(path + "/") for existing in files):
            raise ValueError("source directory conflicts with a file")

    def _changes(self) -> dict[str, bytes]:
        return {path: content for path, content in self._files.items()
                if path not in self._base or self._base[path] != content}

    def proposal(self) -> Proposal | None:
        changes = self._changes()
        return create_proposal(changes) if changes else None

    def tree_digest(self) -> str:
        """Hash the complete loaded snapshot using a versioned, framed format.

        This digest is local workflow evidence, not the verifier's source digest.
        """
        digest = hashlib.sha256(b"hako-coding-agent-tree-v2\0")
        for path, content in sorted(self._files.items()):
            encoded = path.encode("utf-8")
            digest.update(len(encoded).to_bytes(8, "big"))
            digest.update(encoded)
            digest.update(b"\1" if path in self._executable else b"\0")
            digest.update(len(content).to_bytes(8, "big"))
            digest.update(content)
        return digest.hexdigest()
