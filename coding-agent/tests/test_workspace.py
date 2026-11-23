import unittest
from unittest.mock import patch

from hako_coding_agent.workspace import Workspace


class WorkspaceTest(unittest.TestCase):
    def test_changes_are_scoped_and_reverting_removes_proposal(self):
        original = {"src/main.py": b"old\n"}
        workspace = Workspace(original)
        before = workspace.tree_digest()
        original["src/main.py"] = b"external mutation"
        self.assertEqual(workspace.read_file("src/main.py"), b"old\n")
        workspace.write_file("src/main.py", b"new\n")
        workspace.write_file("new.py", b"new file\n")
        self.assertEqual(workspace.list_files(), ("new.py", "src/main.py"))
        self.assertNotEqual(workspace.tree_digest(), before)
        self.assertIsNotNone(workspace.proposal())
        workspace.write_file("src/main.py", b"old\n")
        self.assertIsNotNone(workspace.proposal())

        other = Workspace({"src/main.py": b"old\n"})
        other.write_file("src/main.py", b"new\n")
        other.write_file("src/main.py", b"old\n")
        self.assertEqual(other.tree_digest(), before)
        self.assertIsNone(other.proposal())

    def test_attempt_workspaces_do_not_share_mutations(self):
        source = {"src/main.py": b"base\n"}
        first_attempt = Workspace(source)
        second_attempt = Workspace(source)
        first_attempt.write_file("src/main.py", b"first attempt\n")
        self.assertEqual(first_attempt.read_file("src/main.py"), b"first attempt\n")
        self.assertEqual(second_attempt.read_file("src/main.py"), b"base\n")
        self.assertEqual(source["src/main.py"], b"base\n")

    def test_rejects_unsafe_writes_without_mutation(self):
        workspace = Workspace({"main.py": b"original"})
        before = workspace.tree_digest()
        for path, content in (("../escape", b"x"), (".github/a", b"x"),
                              ("main.py", b"AKIAexample"), ("main.py", b"\xff")):
            with self.subTest(path=path), self.assertRaises(ValueError):
                workspace.write_file(path, content)
            self.assertEqual(workspace.tree_digest(), before)

    def test_change_limit_does_not_mutate_workspace(self):
        workspace = Workspace({"a": b"a"})
        with patch("hako_coding_agent.workspace.MAX_FILES", 1):
            workspace.write_file("a", b"b")
            before = workspace.tree_digest()
            with self.assertRaises(ValueError):
                workspace.write_file("b", b"b")
            self.assertEqual(workspace.tree_digest(), before)
            self.assertEqual(workspace.list_files(), ("a",))

    def test_new_empty_file_counts_toward_change_limit(self):
        workspace = Workspace({"a": b"a"})
        with patch("hako_coding_agent.workspace.MAX_FILES", 1):
            workspace.write_file("a", b"b")
            with self.assertRaises(ValueError):
                workspace.write_file("empty", b"")

    def test_digest_distinguishes_framed_file_contents(self):
        self.assertNotEqual(Workspace({"a": b"bc"}).tree_digest(),
                            Workspace({"ab": b"c"}).tree_digest())

    def test_rejects_file_directory_conflicts(self):
        with self.assertRaises(ValueError):
            Workspace({"a": b"x", "a/b": b"y"})
        workspace = Workspace({"a/b": b"x"})
        before = workspace.tree_digest()
        with self.assertRaises(ValueError):
            workspace.write_file("a", b"y")
        self.assertEqual(workspace.tree_digest(), before)


if __name__ == "__main__":
    unittest.main()
