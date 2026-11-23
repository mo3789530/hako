"""Versioned workspace checkpoints bound to an authorized source and attempt.

Storage, integrity protection, authorization and exclusive leases are supplied
by the caller. A checkpoint is not a verified result or invocation fence.
"""

import base64
import binascii
import hashlib
import hmac
import json
import re
import sqlite3
from dataclasses import asdict

from .execution import Attempt
from .proposal import MAX_FILES
from .source import load_source
from .workspace import MAX_CHANGED_BYTES, Workspace

MAX_CHECKPOINT_BYTES = 12 << 20


def encode_workspace(attempt: Attempt, workspace: Workspace) -> bytes:
    """Encode changes; callers must store the result confidentially and durably."""
    attempt.validate()
    payload = {
        "version": 1,
        "attempt": asdict(attempt),
        "tree_digest": workspace.tree_digest(),
        "changes": {
            path: base64.b64encode(content).decode("ascii")
            for path, content in sorted(workspace.export_changes().items())
        },
    }
    data = json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
    if len(data) > MAX_CHECKPOINT_BYTES:
        raise ValueError("checkpoint too large")
    return data


def restore_workspace(attempt: Attempt, source_tar: bytes, data: bytes) -> Workspace:
    """Restore changes onto the digest-pinned source, validating every write."""
    attempt.validate()
    if not isinstance(data, bytes) or len(data) > MAX_CHECKPOINT_BYTES:
        raise ValueError("invalid checkpoint")
    try:
        payload = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("invalid checkpoint") from exc
    if (
        not isinstance(payload, dict)
        or set(payload) != {"version", "attempt", "tree_digest", "changes"}
        or type(payload["version"]) is not int
        or payload["version"] != 1
        or payload["attempt"] != asdict(attempt)
        or not isinstance(payload["changes"], dict)
        or len(payload["changes"]) > MAX_FILES
        or not isinstance(payload["tree_digest"], str)
    ):
        raise ValueError("invalid checkpoint identity or format")
    workspace = load_source(source_tar, attempt.source_sha256)
    total = 0
    for path, encoded in payload["changes"].items():
        if not isinstance(path, str) or not isinstance(encoded, str):
            raise ValueError("invalid checkpoint change")
        try:
            content = base64.b64decode(encoded, validate=True)
        except (binascii.Error, ValueError) as exc:
            raise ValueError("invalid checkpoint change") from exc
        total += len(content)
        if total > MAX_CHANGED_BYTES:
            raise ValueError("checkpoint changes too large")
        workspace.write_file(path, content)
    digest = payload["tree_digest"]
    if not re.fullmatch(r"[0-9a-f]{64}", digest) or not hmac.compare_digest(
        workspace.tree_digest(), digest
    ):
        raise ValueError("checkpoint tree digest mismatch")
    return workspace


def checkpoint_digest(data: bytes) -> str:
    """Return a digest for a trusted storage manifest to bind checkpoint bytes."""
    return hashlib.sha256(data).hexdigest()


def _attempt_key(attempt: Attempt) -> str:
    """Derive storage namespace from the complete trusted attempt identity."""
    attempt.validate()
    identity = json.dumps(asdict(attempt), sort_keys=True, separators=(",", ":"))
    return hashlib.sha256(identity.encode("utf-8")).hexdigest()


def _check_stored_identity(attempt: Attempt, data: bytes) -> None:
    try:
        payload = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("invalid checkpoint") from exc
    if not isinstance(payload, dict) or payload.get("attempt") != asdict(attempt):
        raise ValueError("checkpoint identity mismatch")


class SQLiteCheckpointStore:
    """Local checkpoint storage with revision checks; caller owns the connection and access control.

    This is not a distributed lease, cancellation fence, or encrypted storage.
    """

    def __init__(self, connection: sqlite3.Connection):
        self.connection = connection
        with connection:
            connection.execute(
                "CREATE TABLE IF NOT EXISTS agent_checkpoints "
                "(key TEXT PRIMARY KEY, revision INTEGER NOT NULL, data BLOB NOT NULL)"
            )

    def load(self, attempt: Attempt) -> tuple[int, bytes] | None:
        key = _attempt_key(attempt)
        row = self.connection.execute(
            "SELECT revision, data FROM agent_checkpoints WHERE key = ?", (key,)
        ).fetchone()
        if row is None:
            return None
        data = bytes(row[1])
        _check_stored_identity(attempt, data)
        return int(row[0]), data

    def save(self, attempt: Attempt, data: bytes, expected_revision: int) -> int:
        key = _attempt_key(attempt)
        if (
            not isinstance(data, bytes)
            or len(data) > MAX_CHECKPOINT_BYTES
            or type(expected_revision) is not int
            or expected_revision < 0
        ):
            raise ValueError("invalid checkpoint storage request")
        _check_stored_identity(attempt, data)
        with self.connection:
            if expected_revision == 0:
                cursor = self.connection.execute(
                    "INSERT OR IGNORE INTO agent_checkpoints VALUES (?, 1, ?)", (key, data)
                )
            else:
                cursor = self.connection.execute(
                    "UPDATE agent_checkpoints SET revision = revision + 1, data = ? "
                    "WHERE key = ? AND revision = ?",
                    (data, key, expected_revision),
                )
            if cursor.rowcount != 1:
                raise ValueError("checkpoint revision conflict")
        return expected_revision + 1

    def delete(self, attempt: Attempt, expected_revision: int) -> None:
        key = _attempt_key(attempt)
        if type(expected_revision) is not int or expected_revision < 1:
            raise ValueError("invalid checkpoint revision")
        with self.connection:
            cursor = self.connection.execute(
                "DELETE FROM agent_checkpoints WHERE key = ? AND revision = ?",
                (key, expected_revision),
            )
            if cursor.rowcount != 1:
                raise ValueError("checkpoint revision conflict")
