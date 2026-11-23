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

    def test(self, iteration):
        self.calls.append(("test", iteration))

    def verify(self):
        self.calls.append(("verify",))
        return next(self.verifications)

    def review(self):
        self.calls.append(("review",))
        return next(self.reviews)


class WorkflowTest(unittest.TestCase):
    def test_failed_test_replans_and_success_requires_matching_tree(self):
        steps = FakeSteps(
            [Verification(1, A), Verification(0, B)], [Review(True, A), Review(True, B)]
        )
        result = run(steps)
        self.assertEqual(
            (result.outcome, result.iterations, result.tree_digest), (Outcome.VERIFIED, 2, B)
        )
        self.assertEqual(
            [x[0] for x in steps.calls], ["plan", "code", "test", "verify", "review"] * 2
        )

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
        for verification, review in (
            (Verification(False, A), Review(True, A)),
            (Verification(0, "bad"), Review(True, A)),
            (Verification(0, A), Review(1, A)),
        ):
            with (
                self.subTest(verification=verification, review=review),
                self.assertRaises(ValueError),
            ):
                run(FakeSteps([verification], [review]))

    def test_maximum_iterations_complete_without_graph_recursion_failure(self):
        steps = FakeSteps([Verification(1, A)] * 10, [Review(True, A)] * 10)
        result = run(steps, max_iterations=10)
        self.assertEqual((result.outcome, result.iterations), (Outcome.FAILED, 10))
        self.assertEqual(len(steps.calls), 50)

    def test_changed_tree_retries_and_can_succeed(self):
        steps = FakeSteps(
            [Verification(0, A), Verification(0, B)], [Review(True, B), Review(True, B)]
        )
        result = run(steps, max_iterations=2)
        self.assertEqual(
            (result.outcome, result.iterations, result.tree_digest), (Outcome.VERIFIED, 2, B)
        )

    def test_adapter_error_stops_workflow(self):
        class FailingSteps(FakeSteps):
            def verify(self):
                raise RuntimeError("verification failed")

        steps = FailingSteps([], [])
        with self.assertRaisesRegex(RuntimeError, "verification failed"):
            run(steps)
        self.assertEqual(steps.calls, [("plan", 1), ("code", 1), ("test", 1)])

    def test_observer_receives_stage_status_without_task_contents(self):
        events = []
        steps = FakeSteps([Verification(0, A)], [Review(True, A)])
        result = run(steps, observe=lambda *event: events.append(event))
        self.assertEqual(result.outcome, Outcome.VERIFIED)
        self.assertEqual(
            [event[:3] for event in events],
            [(stage, 1, "completed") for stage in ("plan", "code", "test", "verify", "review")],
        )
        self.assertTrue(all(isinstance(event[3], int) and event[3] >= 0 for event in events))

    def test_observer_reports_failed_stage_and_propagates_error(self):
        class FailingSteps(FakeSteps):
            def code(self, iteration):
                raise RuntimeError("private detail")

        events = []
        with self.assertRaises(RuntimeError):
            run(FailingSteps([], []), observe=lambda *event: events.append(event))
        self.assertEqual(events[-1][:3], ("code", 1, "failed"))
        self.assertNotIn("private detail", str(events))


if __name__ == "__main__":
    unittest.main()
