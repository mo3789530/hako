import hashlib
import io
import json
import sqlite3
import tarfile
from contextlib import closing
from dataclasses import replace

import pytest

from hako_coding_agent.checkpoint import (
    SQLiteCheckpointStore,
    checkpoint_digest,
    encode_workspace,
    restore_workspace,
)
from hako_coding_agent.execution import Attempt
from hako_coding_agent.source import load_source


def source_tar():
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w") as archive:
        for path, content, mode in (("main.py", b"old", 0o644), ("run.sh", b"script", 0o755)):
            member = tarfile.TarInfo(path)
            member.size = len(content)
            member.mode = mode
            archive.addfile(member, io.BytesIO(content))
    return buffer.getvalue()


def inputs():
    source = source_tar()
    attempt = Attempt(
        "tenant", "run", "attempt", "plane", 1, 1, "a" * 40, hashlib.sha256(source).hexdigest()
    )
    return attempt, source


def test_workspace_checkpoint_round_trip_preserves_base_and_executable_protection():
    attempt, source = inputs()
    workspace = load_source(source, attempt.source_sha256)
    workspace.write_file("main.py", b"new")
    workspace.write_file("test_main.py", b"test")
    data = encode_workspace(attempt, workspace)
    restored = restore_workspace(attempt, source, data)
    assert checkpoint_digest(data) == hashlib.sha256(data).hexdigest()
    assert restored.tree_digest() == workspace.tree_digest()
    assert restored.read_base_file("main.py") == b"old"
    assert restored.list_changes() == ("main.py", "test_main.py")
    with pytest.raises(ValueError):
        restored.write_file("run.sh", b"changed")


def test_workspace_checkpoint_rejects_stale_attempt_and_tampering():
    attempt, source = inputs()
    workspace = load_source(source, attempt.source_sha256)
    workspace.write_file("main.py", b"new")
    data = encode_workspace(attempt, workspace)
    with pytest.raises(ValueError):
        restore_workspace(replace(attempt, generation=2), source, data)
    for changes in ({"main.py": "bm90IG5ldw=="}, {"../unsafe": "eA=="}, {"main.py": "!"}):
        payload = json.loads(data)
        payload["changes"] = changes
        with pytest.raises(ValueError):
            restore_workspace(attempt, source, json.dumps(payload).encode())
    with pytest.raises(ValueError):
        restore_workspace(attempt, b"wrong", data)


def test_sqlite_checkpoint_storage_survives_reopen_and_rejects_stale_writes(tmp_path):
    path = tmp_path / "checkpoints.db"
    attempt, source = inputs()
    workspace = load_source(source, attempt.source_sha256)
    workspace.write_file("main.py", b"changed")
    data = encode_workspace(attempt, workspace)
    with closing(sqlite3.connect(path)) as connection:
        store = SQLiteCheckpointStore(connection)
        assert store.save(attempt, data, 0) == 1
        with pytest.raises(ValueError, match="revision conflict"):
            store.save(attempt, data, 0)
        for other in (replace(attempt, tenant_id="other"), replace(attempt, repository_id=2)):
            assert store.load(other) is None
            with pytest.raises(ValueError, match="identity mismatch"):
                store.save(other, data, 0)
            with pytest.raises(ValueError, match="revision conflict"):
                store.delete(other, 1)
    with closing(sqlite3.connect(path)) as connection:
        store = SQLiteCheckpointStore(connection)
        revision, saved = store.load(attempt)
        assert restore_workspace(attempt, source, saved).tree_digest() == workspace.tree_digest()
        assert store.save(attempt, saved, revision) == 2
        with pytest.raises(ValueError, match="revision conflict"):
            store.save(attempt, saved, revision)
        with pytest.raises(ValueError, match="revision conflict"):
            store.delete(attempt, revision)
        store.delete(attempt, 2)
        assert store.load(attempt) is None
