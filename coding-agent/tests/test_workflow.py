import unittest

from hako_coding_agent.workflow import Outcome, Review, Verification, run

A = "a" * 64
B = "b" * 64

class FakeSteps:
    def __init__(self, verifications, reviews):
        self.verifications = iter(verifications)
        self.reviews = iter(reviews)
        self.calls = []

    def plan(self, iteration):
        self.calls.append(("plan", iteration))

    def code(self, iteration):
        self.calls.append(("code", iteration))

    def verify(self):
        self.calls.append(("verify",))
        return next(self.verifications)

    def review(self):
        self.calls.append(("review",))
        return next(self.reviews)


class WorkflowTest(unittest.TestCase):
    def test_failed_test_replans_and_success_requires_matching_tree(self):
        steps = FakeSteps([Verification(1, A), Verification(0, B)],
                          [Review(True, A), Review(True, B)])
        result = run(steps)
        self.assertEqual((result.outcome, result.iterations, result.tree_digest),
                         (Outcome.VERIFIED, 2, B))
        self.assertEqual([x[0] for x in steps.calls],
                         ["plan", "code", "verify", "review"] * 2)

    def test_review_edit_invalidates_verification(self):
        steps = FakeSteps([Verification(0, A)], [Review(True, B)])
        self.assertEqual(run(steps, max_iterations=1).outcome, Outcome.UNVERIFIED)

    def test_missing_test_is_unverified(self):
        steps = FakeSteps([Verification(None, A)], [Review(True, A)])
        self.assertEqual(run(steps).outcome, Outcome.UNVERIFIED)

    def test_veto_exhausts_budget(self):
        steps = FakeSteps([Verification(0, A)], [Review(False, A)])
        self.assertEqual(run(steps, max_iterations=1).outcome, Outcome.FAILED)

    def test_invalid_iteration_budget(self):
        for value in (0, 11, True):
            with self.subTest(value=value), self.assertRaises(ValueError):
                run(FakeSteps([], []), max_iterations=value)

    def test_invalid_evidence_is_rejected(self):
        for verification, review in ((Verification(False, A), Review(True, A)),
                                     (Verification(0, "bad"), Review(True, A)),
                                     (Verification(0, A), Review(1, A))):
            with self.subTest(verification=verification, review=review), self.assertRaises(ValueError):
                run(FakeSteps([verification], [review]))


if __name__ == "__main__":
    unittest.main()
