import io
import unittest
import zipfile

from hako_coding_agent.agent import run_agent
from hako_coding_agent.languages import detect_languages
from hako_coding_agent.workflow import Outcome
from hako_coding_agent.workspace import Workspace


class ScriptedModel:
    def __init__(self, path, content):
        self.path = path
        self.content = content

    def plan(self, task, workspace, iteration):
        return "Update the requested file"

    def code(self, task, plan, workspace):
        workspace.write_file(self.path, self.content)

    def review(self, task, workspace):
        return True


class StubVerifier:
    def verify(self, workspace):
        return 0


class LanguagesTest(unittest.TestCase):
    def test_detect_mixed_and_manifest_only_projects(self):
        paths = ["apps/server/pom.xml", "tools/pyproject.toml", "go.mod", "lib/Cargo.toml",
                 "ui/tsconfig.json", "module/main.tf", "module/vars.tfvars"]
        self.assertEqual(detect_languages(paths), ("Go", "Java", "Python", "Rust", "Terraform", "TypeScript"))
        self.assertEqual(detect_languages(["README.md", "package.json"]), ())

    def test_changes_become_proposals_for_each_language(self):
        cases = [("main.py", b"print(2)\n"), ("Main.java", b"class Main {}\n"),
                 ("main.go", b"package main\n"), ("src/main.rs", b"fn main() {}\n"),
                 ("src/main.tsx", b"export const x = 2;\n")]
        for path, content in cases:
            with self.subTest(path=path):
                workspace = Workspace({path: b"old\n"})
                result = run_agent("Update source", workspace, ScriptedModel(path, content), StubVerifier())
                self.assertEqual(result.workflow.outcome, Outcome.VERIFIED)
                with zipfile.ZipFile(io.BytesIO(result.proposal.data)) as archive:
                    self.assertEqual(archive.read(path), content)

    def test_terraform_protected_paths_remain_unwritable(self):
        workspace = Workspace({"infra/main.tf": b"original", "main.tf": b"original"})
        self.assertEqual(detect_languages(["infra/main.tf", "main.tf"]), ("Terraform",))
        self.assertEqual(workspace.list_files(), ())
        with self.assertRaises(ValueError):
            workspace.write_file("infra/main.tf", b"changed")
        self.assertNotIn("infra/main.tf", workspace.list_files())
        for path in ("main.tf", "module/vars.tfvars", "nested/config.TF.JSON", ".terraform.lock.hcl"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                workspace.write_file(path, b"changed")
        self.assertEqual(workspace.list_changes(), ())


if __name__ == "__main__":
    unittest.main()
