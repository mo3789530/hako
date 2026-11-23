import unittest
from unittest.mock import patch

from hako_coding_agent.workspace import Workspace, validate_source_path


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

    def test_rejects_unsafe_writes_without_mutation(self):
        workspace = Workspace({"main.py": b"original"})
        before = workspace.tree_digest()
        for path, content in (
            ("../escape", b"x"),
            (".github/a", b"x"),
            ("main.py", b"AKIAexample"),
            ("main.py", b"\xff"),
        ):
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
        self.assertNotEqual(
            Workspace({"a": b"bc"}).tree_digest(), Workspace({"ab": b"c"}).tree_digest()
        )

    def test_rejects_file_directory_conflicts(self):
        with self.assertRaises(ValueError):
            Workspace({"a": b"x", "a/b": b"y"})
        workspace = Workspace({"a/b": b"x"})
        before = workspace.tree_digest()
        with self.assertRaises(ValueError):
            workspace.write_file("a", b"y")
        self.assertEqual(workspace.tree_digest(), before)

    def test_source_paths_reject_traversal_and_unrepresentable_names(self):
        for path in ("", "/absolute", "a//b", "a/../b", "a\\b", "a\nb", "\ud800"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                validate_source_path(path)
                Workspace({path: b"x"})

    def test_constructor_enforces_source_size_and_executable_membership(self):
        with patch("hako_coding_agent.workspace.MAX_SOURCE_FILE_BYTES", 1):
            with self.assertRaises(ValueError):
                Workspace({"a": b"xx"})
        with patch("hako_coding_agent.workspace.MAX_SOURCE_BYTES", 2):
            with self.assertRaises(ValueError):
                Workspace({"a": b"x", "b": b"y", "c": b"z"})
        for executable in (frozenset({"missing"}), {"a"}):
            with self.subTest(executable=executable), self.assertRaises(ValueError):
                Workspace({"a": b"x"}, executable=executable)

    def test_text_tools_reject_binary_and_oversized_source(self):
        workspace = Workspace({"nul": b"a\0b", "invalid": b"\xff", "large": b"x" * 5})
        for path in ("nul", "invalid"):
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, "text tool"):
                workspace.read_file(path)
        with patch("hako_coding_agent.workspace.MAX_FILE_BYTES", 4):
            with self.assertRaisesRegex(ValueError, "text tool"):
                workspace.read_file("large")

    def test_protected_source_is_hidden_from_both_read_tools(self):
        workspace = Workspace({".github/workflow.yml": b"secret", "src/main.py": b"ok"})
        self.assertEqual(workspace.list_files(), ("src/main.py",))
        for read in (workspace.read_file, workspace.read_base_file):
            with self.assertRaises(ValueError):
                read(".github/workflow.yml")

    def test_changed_byte_limit_rejects_without_mutating(self):
        workspace = Workspace({"a": b"old"})
        before = workspace.tree_digest()
        with patch("hako_coding_agent.workspace.MAX_CHANGED_BYTES", 2):
            with self.assertRaisesRegex(ValueError, "change limit"):
                workspace.write_file("a", b"new")
        self.assertEqual(workspace.tree_digest(), before)


if __name__ == "__main__":
    unittest.main()
