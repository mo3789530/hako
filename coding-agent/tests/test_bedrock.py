import copy
import unittest

from hako_coding_agent.agent import run_agent
from hako_coding_agent.bedrock import BedrockCodingModel
from hako_coding_agent.workflow import Outcome
from hako_coding_agent.workspace import Workspace


def text_response(text):
    return {
        "stopReason": "end_turn",
        "output": {"message": {"role": "assistant", "content": [{"text": text}]}},
    }


def tool_response(name, arguments, call_id="call1"):
    return {
        "stopReason": "tool_use",
        "output": {
            "message": {
                "role": "assistant",
                "content": [{"toolUse": {"toolUseId": call_id, "name": name, "input": arguments}}],
            }
        },
    }


class Client:
    def __init__(self, responses):
        self.responses = iter(responses)
        self.requests = []

    def converse(self, **kwargs):
        self.requests.append(copy.deepcopy(kwargs))
        return next(self.responses)


class Verifier:
    def verify(self, workspace):
        return 0 if workspace.read_file("app.py") == b"new\n" else 1


class BedrockTest(unittest.TestCase):
    def test_model_tool_call_to_proposal(self):
        client = Client(
            [
                text_response("Edit app.py"),
                tool_response("write_file", {"path": "app.py", "content": "new\n"}),
                text_response("Done"),
                text_response("Tests updated"),
                text_response("PASS"),
            ]
        )
        model = BedrockCodingModel(client, "test-model")
        result = run_agent("Update app", Workspace({"app.py": b"old\n"}), model, Verifier())
        self.assertEqual(result.workflow.outcome, Outcome.VERIFIED)
        self.assertIsNotNone(result.proposal)
        sent = client.requests[2]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(sent["toolUseId"], "call1")
        self.assertEqual(sent["content"][0]["json"], {"written": True})
        self.assertNotIn(
            "write_file", [t["toolSpec"]["name"] for t in client.requests[0]["toolConfig"]["tools"]]
        )

    def test_review_cannot_write_even_if_model_requests_it(self):
        client = Client(
            [tool_response("write_file", {"path": "a", "content": "x"}), text_response("FAIL")]
        )
        workspace = Workspace({"a": b"old"})
        self.assertFalse(BedrockCodingModel(client, "model").review("task", workspace))
        self.assertEqual(workspace.read_file("a"), b"old")
        result = client.requests[1]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(result["status"], "error")

    def test_missing_file_returns_sanitized_tool_error_and_model_recovers(self):
        client = Client(
            [
                tool_response("read_file", {"path": "missing-secret"}),
                tool_response("list_files", {}, "call2"),
                text_response("A plan"),
            ]
        )
        model = BedrockCodingModel(client, "model")
        self.assertEqual(model.plan("task", Workspace({"app.py": b"data"}), 1), "A plan")
        result = client.requests[1]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(result["status"], "error")
        self.assertNotIn("missing-secret", str(result))

    def test_tool_budget_and_duplicate_ids_fail_closed(self):
        for responses in (
            [tool_response("list_files", {}), tool_response("list_files", {}, "call2")],
            [tool_response("list_files", {}), tool_response("list_files", {})],
        ):
            with self.subTest(responses=responses), self.assertRaises(ValueError):
                BedrockCodingModel(
                    Client(responses),
                    "model",
                    max_tool_calls=1 if responses[1] != responses[0] else 3,
                ).plan("task", Workspace({}), 1)

    def test_abnormal_stops_and_missing_verdict_fail_closed(self):
        for response in (
            {
                "stopReason": "max_tokens",
                "output": {"message": {"role": "assistant", "content": [{"text": "PASS"}]}},
            },
            text_response("looks good"),
        ):
            with self.subTest(response=response), self.assertRaises(ValueError):
                BedrockCodingModel(Client([response]), "model").review("task", Workspace({}))

    def test_detected_languages_are_advisory_model_context(self):
        client = Client([text_response("Plan")])
        BedrockCodingModel(client, "model").plan(
            "task", Workspace({"main.tf": b"", "main.go": b""}), 1
        )
        prompt = client.requests[0]["messages"][0]["content"][0]["text"]
        self.assertIn("Detected source languages (advisory): Go, Terraform", prompt)
        self.assertIn("Go: Inspect go.mod", prompt)
        self.assertIn("Terraform: Inspect modules", prompt)
        self.assertNotIn("Python: Inspect", prompt)

    def test_plan_includes_guidance_for_each_detected_language(self):
        client = Client([text_response("Plan")])
        workspace = Workspace(
            {
                name: b""
                for name in ("main.tf", "main.go", "main.rs", "main.ts", "main.py", "Main.java")
            }
        )
        BedrockCodingModel(client, "model").plan("task", workspace, 1)
        prompt = client.requests[0]["messages"][0]["content"][0]["text"]
        for language in ("Terraform", "Go", "Rust", "TypeScript", "Python", "Java"):
            with self.subTest(language=language):
                self.assertIn(f"{language}:", prompt)
        self.assertIn("concrete test cases", prompt)

    def test_test_stage_can_write_tests_without_claiming_execution(self):
        client = Client(
            [
                tool_response("write_file", {"path": "test_app.py", "content": "assert True\n"}),
                text_response("Added a test"),
            ]
        )
        workspace = Workspace({"app.py": b"x"})
        BedrockCodingModel(client, "model").test("task", "plan", workspace)
        self.assertEqual(workspace.read_file("test_app.py"), b"assert True\n")
        prompt = client.requests[0]["messages"][0]["content"][0]["text"]
        self.assertIn("Do not claim tests were executed", prompt)

    def test_rejects_invalid_model_configuration(self):
        for client, model_id, options in (
            (Client([]), "", {}),
            (Client([]), "model", {"max_turns": 0}),
            (Client([]), "model", {"max_turns": True}),
            (Client([]), "model", {"max_tool_calls": 129}),
            (Client([]), "model", {"max_tokens": 4097}),
        ):
            with self.subTest(model_id=model_id, options=options), self.assertRaises(ValueError):
                BedrockCodingModel(client, model_id, **options)

    def test_rejects_malformed_model_messages(self):
        malformed = (
            {},
            {"stopReason": "end_turn", "output": {"message": {"role": "user", "content": []}}},
            {"stopReason": "end_turn", "output": {"message": {"role": "assistant", "content": []}}},
            {
                "stopReason": "end_turn",
                "output": {"message": {"role": "assistant", "content": [{"image": "x"}]}},
            },
        )
        for response in malformed:
            with self.subTest(response=response), self.assertRaises(ValueError):
                BedrockCodingModel(Client([response]), "model").plan("task", Workspace({}), 1)

    def test_rejected_tool_arguments_are_sanitized(self):
        client = Client(
            [
                tool_response("write_file", {"path": "../outside", "content": "secret"}),
                text_response("Plan"),
            ]
        )
        self.assertEqual(BedrockCodingModel(client, "model").plan("task", Workspace({}), 1), "Plan")
        result = client.requests[1]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(result["status"], "error")
        self.assertNotIn("outside", str(result))
        self.assertNotIn("secret", str(result))

    def test_turn_budget_fails_closed(self):
        model = BedrockCodingModel(Client([tool_response("list_files", {})]), "model", max_turns=1)
        with self.assertRaisesRegex(ValueError, "turn budget"):
            model.plan("task", Workspace({}), 1)

    def test_prompt_cache_is_opt_in_and_retained_across_tool_turns(self):
        for enabled in (False, True):
            client = Client([tool_response("list_files", {}), text_response("Plan")])
            model = BedrockCodingModel(client, "model", prompt_cache=enabled)
            self.assertEqual(model.plan("task", Workspace({}), 1), "Plan")
            for request in client.requests:
                content = request["messages"][0]["content"]
                points = [block for block in content if "cachePoint" in block]
                self.assertEqual(points, [{"cachePoint": {"type": "default"}}] if enabled else [])

    def test_prompt_cache_requires_explicit_boolean(self):
        with self.assertRaises(ValueError):
            BedrockCodingModel(Client([]), "model", prompt_cache=1)


if __name__ == "__main__":
    unittest.main()
