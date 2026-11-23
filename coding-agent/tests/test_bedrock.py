import copy
import unittest

from hako_coding_agent.agent import run_agent
from hako_coding_agent.bedrock import BedrockCodingModel
from hako_coding_agent.workflow import Outcome
from hako_coding_agent.workspace import Workspace


def text_response(text):
    return {"stopReason": "end_turn", "output": {"message": {"role": "assistant", "content": [{"text": text}]}}}


def tool_response(name, arguments, call_id="call1"):
    return {"stopReason": "tool_use", "output": {"message": {"role": "assistant", "content": [
        {"toolUse": {"toolUseId": call_id, "name": name, "input": arguments}}]}}}


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
        client = Client([text_response("Edit app.py"),
                         tool_response("write_file", {"path": "app.py", "content": "new\n"}),
                         text_response("Done"), text_response("PASS")])
        model = BedrockCodingModel(client, "test-model")
        result = run_agent("Update app", Workspace({"app.py": b"old\n"}), model, Verifier())
        self.assertEqual(result.workflow.outcome, Outcome.VERIFIED)
        self.assertIsNotNone(result.proposal)
        sent = client.requests[2]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(sent["toolUseId"], "call1")
        self.assertEqual(sent["content"][0]["json"], {"written": True})
        self.assertNotIn("write_file", [t["toolSpec"]["name"] for t in client.requests[0]["toolConfig"]["tools"]])

    def test_review_cannot_write_even_if_model_requests_it(self):
        client = Client([tool_response("write_file", {"path": "a", "content": "x"}), text_response("FAIL")])
        workspace = Workspace({"a": b"old"})
        self.assertFalse(BedrockCodingModel(client, "model").review("task", workspace))
        self.assertEqual(workspace.read_file("a"), b"old")
        result = client.requests[1]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(result["status"], "error")

    def test_missing_file_returns_sanitized_tool_error_and_model_recovers(self):
        client = Client([tool_response("read_file", {"path": "missing-secret"}),
                         tool_response("list_files", {}, "call2"), text_response("A plan")])
        model = BedrockCodingModel(client, "model")
        self.assertEqual(model.plan("task", Workspace({"app.py": b"data"}), 1), "A plan")
        result = client.requests[1]["messages"][-1]["content"][0]["toolResult"]
        self.assertEqual(result["status"], "error")
        self.assertNotIn("missing-secret", str(result))

    def test_tool_budget_and_duplicate_ids_fail_closed(self):
        for responses in ([tool_response("list_files", {}), tool_response("list_files", {}, "call2")],
                          [tool_response("list_files", {}), tool_response("list_files", {})]):
            with self.subTest(responses=responses), self.assertRaises(ValueError):
                BedrockCodingModel(Client(responses), "model", max_tool_calls=1 if responses[1] != responses[0] else 3).plan("task", Workspace({}), 1)

    def test_abnormal_stops_and_missing_verdict_fail_closed(self):
        for response in ({"stopReason": "max_tokens", "output": {"message": {"role": "assistant", "content": [{"text": "PASS"}]}}},
                         text_response("looks good")):
            with self.subTest(response=response), self.assertRaises(ValueError):
                BedrockCodingModel(Client([response]), "model").review("task", Workspace({}))

    def test_detected_languages_are_advisory_model_context(self):
        client = Client([text_response("Plan")])
        BedrockCodingModel(client, "model").plan("task", Workspace({"main.tf": b"", "main.go": b""}), 1)
        prompt = client.requests[0]["messages"][0]["content"][0]["text"]
        self.assertIn("Detected source languages (advisory): Go", prompt)


if __name__ == "__main__":
    unittest.main()
