#!/usr/bin/env python3
import gzip
import importlib.util
import json
import pathlib
import tempfile
import unittest

_PATH = pathlib.Path(__file__).with_name("rekey-local-workspace-initrd.py")
_SPEC = importlib.util.spec_from_file_location("local_initrd_rekey", _PATH)
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)


def entry(name, data, mode=0o100644, uid=0, gid=0):
    encoded = name.encode() + b"\0"
    values = [1, mode, uid, gid, 1, 0, len(data), 0, 0, 0, 0, len(encoded), 0]
    header = b"070701" + b"".join(f"{item:08x}".encode() for item in values)
    return header + encoded + _MODULE.padding(110 + len(encoded)) + data + _MODULE.padding(len(data))


def archive(key, extra=()):
    body = entry("etc/hako/workspace-agent-public-keys.json", key)
    body += b"".join(entry(name, data, mode=0o100755 if name in ("usr/local/bin/hako-workspace-agent", "init") else 0o100644)
                     for name, data in extra)
    body += entry("TRAILER!!!", b"")
    return body + b"\0" * ((-len(body)) % 512)


class RekeyTests(unittest.TestCase):
    def test_replaces_exact_image_owned_key_entry(self):
        old = b'{"local_agent":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}\n'
        new = b'{"local_agent":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}\n'
        with tempfile.TemporaryDirectory() as directory:
            source, destination, key_path = (pathlib.Path(directory) / name for name in ("in.gz", "out.gz", "key.json"))
            source.write_bytes(gzip.compress(archive(old)))
            key_path.write_bytes(new)
            _MODULE.rekey(str(source), str(destination), str(key_path))
            with gzip.open(destination, "rb") as result:
                self.assertEqual(result.read(), archive(new))
            self.assertEqual(destination.stat().st_mode & 0o777, 0o600)

    def test_replaces_workspace_agent_and_updates_newc_size(self):
        old = b'{"local_agent":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}\n'
        new = b'{"local_agent":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}\n'
        with tempfile.TemporaryDirectory() as directory:
            source, destination, key_path, agent_path = (pathlib.Path(directory) / name for name in
                ("in.gz", "out.gz", "key.json", "hako-workspace-agent"))
            source.write_bytes(gzip.compress(archive(old, [("usr/local/bin/hako-workspace-agent", b"old-agent")])) )
            key_path.write_bytes(new)
            agent_path.write_bytes(b"replacement-agent-with-different-size")
            agent_path.chmod(0o700)
            _MODULE.rekey(str(source), str(destination), str(key_path), str(agent_path))
            with gzip.open(destination, "rb") as result:
                self.assertEqual(result.read(), archive(new, [("usr/local/bin/hako-workspace-agent", agent_path.read_bytes())]))

    def test_replaces_guest_init_for_bounded_runtime_diagnostics(self):
        old = b'{"local_agent":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}\n'
        new = b'{"local_agent":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}\n'
        with tempfile.TemporaryDirectory() as directory:
            source, destination, key_path, init_path = (pathlib.Path(directory) / name for name in
                ("in.gz", "out.gz", "key.json", "guest-init"))
            source.write_bytes(gzip.compress(archive(old, [("init", b"old-init")])) )
            key_path.write_bytes(new)
            init_path.write_bytes(b"replacement-guest-init-with-different-size")
            init_path.chmod(0o700)
            _MODULE.rekey(str(source), str(destination), str(key_path), guest_init_path=str(init_path))
            with gzip.open(destination, "rb") as result:
                self.assertEqual(result.read(), archive(new, [("init", init_path.read_bytes())]))

    def test_rejects_wrong_key_size_and_duplicate_entries(self):
        valid = b'{"local_agent":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}\n'
        with tempfile.TemporaryDirectory() as directory:
            source, destination, key_path = (pathlib.Path(directory) / name for name in ("in.gz", "out.gz", "key.json"))
            key_path.write_bytes(valid)
            key_path.write_bytes(b'{"local_agent":"' + b"A" * 42 + b'"}\n')
            with self.assertRaises(ValueError):
                _MODULE.rekey(str(source), str(destination), str(key_path))
            self.assertFalse(destination.exists())
            key_path.write_bytes(valid)
            source.write_bytes(gzip.compress(archive(valid, [("etc/hako/workspace-agent-public-keys.json", valid)])))
            with self.assertRaises(ValueError):
                _MODULE.rekey(str(source), str(destination), str(key_path))
            self.assertFalse(destination.exists())

    def test_rejects_malformed_or_multiple_public_keys(self):
        for data in (b"{}", json.dumps({"local_agent": "bad"}).encode(),
                     json.dumps({"local_agent": "A" * 43, "other": "B" * 43}).encode()):
            with self.subTest(data=data), self.assertRaises(ValueError):
                _MODULE.public_key(data)


if __name__ == "__main__":
    unittest.main()
