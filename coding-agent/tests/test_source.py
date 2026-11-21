import hashlib
import io
import tarfile
import unittest

from hako_coding_agent.source import load_source


def archive(entries):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as writer:
        for name, content, kind, mode in entries:
            item = tarfile.TarInfo(name)
            item.type = kind
            item.mode = mode
            item.size = len(content) if kind == tarfile.REGTYPE else 0
            if kind == tarfile.SYMTYPE:
                item.linkname = "../outside"
            writer.addfile(item, io.BytesIO(content) if kind == tarfile.REGTYPE else None)
    return output.getvalue()


def load(data):
    return load_source(data, hashlib.sha256(data).hexdigest())


class SourceTest(unittest.TestCase):
    def test_load_and_verify_digest(self):
        data = archive([("src/", b"", tarfile.DIRTYPE, 0o755),
                        ("src/main.py", b"print(1)\n", tarfile.REGTYPE, 0o644)])
        workspace = load(data)
        self.assertEqual(workspace.read_file("src/main.py"), b"print(1)\n")
        with self.assertRaisesRegex(ValueError, "digest mismatch"):
            load_source(data, "0" * 64)

    def test_reject_unsafe_and_unrepresentable_entries(self):
        invalid = [
            [("../escape", b"x", tarfile.REGTYPE, 0o644)],
            [("a", b"", tarfile.SYMTYPE, 0o777)],
            [("a", b"x", tarfile.REGTYPE, 0o644), ("a", b"y", tarfile.REGTYPE, 0o644)],
            [("a", b"x", tarfile.REGTYPE, 0o644), ("a/b", b"", tarfile.DIRTYPE, 0o755)],
        ]
        for entries in invalid:
            with self.subTest(entries=entries), self.assertRaises(ValueError):
                load(archive(entries))

    def test_protected_binary_and_executable_files_are_retained(self):
        data = archive([(".github/workflow.yml", b"private", tarfile.REGTYPE, 0o644),
                        ("image.png", b"\xff\0", tarfile.REGTYPE, 0o644),
                        ("script.sh", b"echo hi\n", tarfile.REGTYPE, 0o755)])
        workspace = load(data)
        self.assertEqual(workspace.list_files(), ("image.png", "script.sh"))
        self.assertEqual(workspace.read_file("script.sh"), b"echo hi\n")
        for path in (".github/workflow.yml", "image.png"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                workspace.read_file(path)
        with self.assertRaises(ValueError):
            workspace.write_file("script.sh", b"echo bye\n")
        nonexec = load(archive([(".github/workflow.yml", b"private", tarfile.REGTYPE, 0o644),
                                ("image.png", b"\xff\0", tarfile.REGTYPE, 0o644),
                                ("script.sh", b"echo hi\n", tarfile.REGTYPE, 0o644)]))
        self.assertNotEqual(workspace.tree_digest(), nonexec.tree_digest())


if __name__ == "__main__":
    unittest.main()
