import io
import unittest
import zipfile

from hako_coding_agent.agent import propose_changes, run_agent
from hako_coding_agent.workflow import Outcome
from hako_coding_agent.workspace import Workspace


class FakeModel:
    def __init__(self, *, edit_review=False):
        self.edit_review = edit_review

    def plan(self, task, workspace, iteration):
        return "Change the example file"

    def code(self, task, plan, workspace):
        workspace.write_file("example.py", b"print(2)\n")

    def review(self, task, workspace):
        if self.edit_review:
            workspace.write_file("example.py", b"print(3)\n")
        return True


class FakeVerifier:
    def __init__(self, *, mutate=False):
        self.mutate = mutate

    def verify(self, workspace):
        if self.mutate:
            workspace.write_file("example.py", b"print(4)\n")
        return 0


class AgentTest(unittest.TestCase):
    def test_draft_produces_proposal_without_claiming_verification(self):
        draft = propose_changes("Update example", Workspace({"example.py": b"print(1)\n"}), FakeModel())
        self.assertEqual(draft.iterations, 1)
        self.assertEqual(len(draft.tree_digest), 64)
        self.assertIsNotNone(draft.proposal)

    def test_draft_rejects_review_mutation_and_unapproved_changes(self):
        changed_review = propose_changes("Update", Workspace({"example.py": b"before"}),
                                         FakeModel(edit_review=True), max_iterations=1)
        self.assertIsNone(changed_review.proposal)
        unapproved = FakeModel()
        unapproved.review = lambda _task, _workspace: False
        result = propose_changes("Update", Workspace({"example.py": b"before"}),
                                 unapproved, max_iterations=1)
        self.assertIsNone(result.proposal)

    def test_workspace_to_proposal(self):
        workspace = Workspace({"example.py": b"print(1)\n"})
        result = run_agent("Update example", workspace, FakeModel(), FakeVerifier())
        self.assertEqual(result.workflow.outcome, Outcome.VERIFIED)
        self.assertIsNotNone(result.proposal)
        with zipfile.ZipFile(io.BytesIO(result.proposal.data)) as archive:
            self.assertEqual(archive.read("example.py"), b"print(2)\n")

    def test_review_change_invalidates_test(self):
        result = run_agent("Update", Workspace({"example.py": b"before"}),
                           FakeModel(edit_review=True), FakeVerifier(), max_iterations=1)
        self.assertEqual(result.workflow.outcome, Outcome.UNVERIFIED)

    def test_verifier_mutation_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "changed the source"):
            run_agent("Update", Workspace({"example.py": b"before"}),
                      FakeModel(), FakeVerifier(mutate=True))


if __name__ == "__main__":
    unittest.main()
