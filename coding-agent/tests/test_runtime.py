import base64
import hashlib
import http.client
import io
import json
import tarfile
import threading
import unittest
from http.server import ThreadingHTTPServer

from hako_coding_agent.execution import Attempt
from hako_coding_agent.policy import AgentCorePolicy
from hako_coding_agent.runtime import (AgentCoreRuntime, InvocationInputError,
                                       make_handler, parse_invocation)


def source_archive():
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as archive:
        item = tarfile.TarInfo("app.py")
        item.size = 4
        archive.addfile(item, io.BytesIO(b"old\n"))
    return output.getvalue()


class FakeConverse:
    def __init__(self, clock=None):
        self.calls = 0
        self.clock = clock

    def converse(self, **_kwargs):
        self.calls += 1
        if self.clock is not None:
            self.clock[0] += 1
        if self.calls == 1:
            return {"stopReason": "end_turn", "output": {"message": {
                "role": "assistant", "content": [{"text": "update app"}]}}}
        if self.calls == 2:
            return {"stopReason": "tool_use", "output": {"message": {
                "role": "assistant", "content": [{"toolUse": {
                    "toolUseId": "write-1", "name": "write_file",
                    "input": {"path": "app.py", "content": "new\n"}}}]}}}
        if self.calls == 3:
            return {"stopReason": "end_turn", "output": {"message": {
                "role": "assistant", "content": [{"text": "updated"}]}}}
        return {"stopReason": "end_turn", "output": {"message": {
            "role": "assistant", "content": [{"text": "PASS"}]}}}


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.archive = source_archive()
        self.attempt = Attempt("tenant", "run", "attempt", "plane", 1, 42,
                               "a" * 40, hashlib.sha256(self.archive).hexdigest())
        self.request = {"schema_version": 1, "attempt": self.attempt.__dict__,
                        "task": "Update app", "source_archive_base64":
                        base64.b64encode(self.archive).decode("ascii")}
        self.body = json.dumps(self.request).encode()
        self.policy = AgentCorePolicy.from_deployment({
            "HAKO_CODING_AGENT_MODEL_ID": "approved-model",
            "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '["approved-model"]',
            "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1"]',
            "HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1",
            "HAKO_CODING_AGENT_MAX_ITERATIONS": "1",
        })

    def test_parse_valid_request_and_reject_duplicates_unknown_fields_and_wrong_digest(self):
        attempt, archive, task = parse_invocation(self.body)
        self.assertEqual((attempt, archive, task), (self.attempt, self.archive, "Update app"))
        with self.assertRaises(InvocationInputError):
            parse_invocation(self.body[:-1] + b',"extra":true}')
        duplicated = self.body[:-1] + b',"schema_version":1}'
        with self.assertRaises(InvocationInputError):
            parse_invocation(duplicated)
        bad = dict(self.request)
        bad["attempt"] = {**self.request["attempt"], "source_sha256": "0" * 64}
        with self.assertRaises(ValueError):
            AgentCoreRuntime(self.policy, FakeConverse()).invoke(json.dumps(bad).encode())

    def test_invocation_returns_explicit_unverified_proposal_and_digest(self):
        response = json.loads(AgentCoreRuntime(self.policy, FakeConverse()).invoke(self.body))
        self.assertEqual(response["attempt"], self.request["attempt"])
        self.assertEqual(response["draft_status"], "proposal_ready")
        self.assertIsNone(response.get("verified"))
        proposal = base64.b64decode(response["proposal"]["data_base64"], validate=True)
        self.assertEqual(hashlib.sha256(proposal).hexdigest(), response["proposal"]["sha256"])

    def test_model_calls_stop_when_attempt_deadline_expires(self):
        policy = AgentCorePolicy.from_deployment({
            "HAKO_CODING_AGENT_MODEL_ID": "approved-model",
            "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '["approved-model"]',
            "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1"]',
            "HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1",
            "HAKO_CODING_AGENT_MAX_ITERATIONS": "1",
            "HAKO_CODING_AGENT_ATTEMPT_TIMEOUT_SECONDS": "1",
        })
        clock = [0.0]
        client = FakeConverse(clock)
        app = AgentCoreRuntime(policy, client, monotonic=lambda: clock[0])
        with self.assertRaises(TimeoutError):
            app.invoke(self.body)
        self.assertEqual(client.calls, 1)

    def test_http_contract_serves_health_and_invocation_with_safe_errors(self):
        app = AgentCoreRuntime(self.policy, FakeConverse())
        server = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(app))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=3)
            connection.request("GET", "/ping")
            health = connection.getresponse()
            self.assertEqual(health.status, 200)
            self.assertEqual(json.loads(health.read()), {"status": "Healthy"})
            connection.request("GET", "/ping?debug=1")
            missing = connection.getresponse()
            self.assertEqual(missing.status, 404)
            missing.read()
            connection.request("POST", "/invocations", body=self.body,
                               headers={"Content-Type": "application/json"})
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.getheader("Cache-Control"), "no-store")
            self.assertEqual(json.loads(response.read())["draft_status"], "proposal_ready")
            connection.request("POST", "/invocations", body=b"{}",
                               headers={"Content-Type": "application/json"})
            bad = connection.getresponse()
            self.assertEqual(bad.status, 400)
            self.assertEqual(json.loads(bad.read()), {"error": "invalid_invocation"})
            connection.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=3)


if __name__ == "__main__":
    unittest.main()
