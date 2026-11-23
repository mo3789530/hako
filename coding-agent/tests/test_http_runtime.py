import base64
import hashlib
import http.client
import io
import json
import tarfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from unittest.mock import patch

from hako_coding_agent.execution import Attempt
from hako_coding_agent.http_runtime import (
    AgentCoreRuntime,
    InvocationInputError,
    make_handler,
    parse_invocation,
)
from hako_coding_agent.policy import AgentCorePolicy


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
            return {
                "stopReason": "end_turn",
                "output": {"message": {"role": "assistant", "content": [{"text": "update app"}]}},
            }
        if self.calls == 2:
            return {
                "stopReason": "tool_use",
                "output": {
                    "message": {
                        "role": "assistant",
                        "content": [
                            {
                                "toolUse": {
                                    "toolUseId": "write-1",
                                    "name": "write_file",
                                    "input": {"path": "app.py", "content": "new\n"},
                                }
                            }
                        ],
                    }
                },
            }
        if self.calls == 3:
            return {
                "stopReason": "end_turn",
                "output": {"message": {"role": "assistant", "content": [{"text": "updated"}]}},
            }
        return {
            "stopReason": "end_turn",
            "output": {"message": {"role": "assistant", "content": [{"text": "PASS"}]}},
        }


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.archive = source_archive()
        self.attempt = Attempt(
            "tenant",
            "run",
            "attempt",
            "plane",
            1,
            42,
            "a" * 40,
            hashlib.sha256(self.archive).hexdigest(),
        )
        self.request = {
            "schema_version": 1,
            "attempt": self.attempt.__dict__,
            "task": "Update app",
            "source_archive_base64": base64.b64encode(self.archive).decode("ascii"),
        }
        self.body = json.dumps(self.request).encode()
        self.policy = AgentCorePolicy.from_deployment(
            {
                "HAKO_CODING_AGENT_MODEL_ID": "approved-model",
                "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '["approved-model"]',
                "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1"]',
                "HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1",
                "HAKO_CODING_AGENT_MAX_ITERATIONS": "1",
            }
        )

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
        policy = AgentCorePolicy.from_deployment(
            {
                "HAKO_CODING_AGENT_MODEL_ID": "approved-model",
                "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '["approved-model"]',
                "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1"]',
                "HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1",
                "HAKO_CODING_AGENT_MAX_ITERATIONS": "1",
                "HAKO_CODING_AGENT_ATTEMPT_TIMEOUT_SECONDS": "1",
            }
        )
        clock = [0.0]
        client = FakeConverse(clock)
        app = AgentCoreRuntime(policy, client, monotonic=lambda: clock[0])
        with self.assertRaises(TimeoutError):
            app.invoke(self.body)
        self.assertEqual(client.calls, 1)

    def test_invalid_invocation_inputs_are_rejected(self):
        invalid_bodies = [
            b"",
            b"\xff",
            b"{",
            b'{"x": NaN}',
            b"[]",
            json.dumps({**self.request, "schema_version": True}).encode(),
            json.dumps({**self.request, "attempt": {}}).encode(),
            json.dumps(
                {**self.request, "attempt": {**self.request["attempt"], "generation": 0}}
            ).encode(),
            json.dumps({**self.request, "task": ""}).encode(),
            json.dumps({**self.request, "source_archive_base64": ""}).encode(),
            json.dumps({**self.request, "source_archive_base64": "!"}).encode(),
        ]
        for body in invalid_bodies:
            with self.subTest(body=body), self.assertRaises(InvocationInputError):
                parse_invocation(body)
        with self.assertRaises(ValueError):
            AgentCoreRuntime(None, FakeConverse())
        with self.assertRaises(ValueError):
            make_handler(None)

    def test_http_rejects_invalid_requests_and_redacts_failures(self):
        class FailingApp:
            def invoke(self, body):
                if body == b"timeout":
                    raise TimeoutError("private")
                raise RuntimeError("private")

        server = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(FailingApp()))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            for path, body, headers, expected in [
                ("/unknown", b"", {}, 404),
                ("/invocations", b"", {"Content-Type": "application/json"}, 413),
                ("/invocations", b"x", {"Content-Type": "text/plain"}, 415),
                ("/invocations", b"x", {"Transfer-Encoding": "chunked"}, 400),
                ("/invocations", b"timeout", {"Content-Type": "application/json"}, 504),
                ("/invocations", b"secret", {"Content-Type": "application/json"}, 500),
            ]:
                with self.subTest(expected=expected):
                    connection = http.client.HTTPConnection(
                        "127.0.0.1", server.server_port, timeout=3
                    )
                    connection.request("POST", path, body=body, headers=headers)
                    response = connection.getresponse()
                    self.assertEqual(response.status, expected)
                    self.assertNotIn(b"private", response.read())
                    connection.close()
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=3)

    def test_runtime_returns_no_proposal_and_rejects_large_response(self):
        from hako_coding_agent.agent import ProposalDraft
        from hako_coding_agent.execution import ProposalExecutionResult

        app = AgentCoreRuntime(self.policy, FakeConverse())
        result = ProposalExecutionResult(self.attempt, ProposalDraft(1, "a" * 64, None))
        with patch("hako_coding_agent.http_runtime.propose_with_policy", return_value=result):
            self.assertEqual(json.loads(app.invoke(self.body))["draft_status"], "no_proposal")
            with patch("hako_coding_agent.http_runtime.MAX_RESPONSE_BYTES", 1):
                with self.assertRaises(ValueError):
                    app.invoke(self.body)

    def test_create_bedrock_client_uses_policy_and_limits(self):
        from hako_coding_agent.http_runtime import create_bedrock_client

        with self.assertRaises(ValueError):
            create_bedrock_client(None)
        with patch("boto3.client") as create:
            client = create_bedrock_client(self.policy)
            self.assertIs(client, create.return_value)
            self.assertEqual(create.call_args.kwargs["region_name"], "ap-northeast-1")
            self.assertEqual(create.call_args.kwargs["config"].read_timeout, 60)

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
            connection.request(
                "POST", "/invocations", body=self.body, headers={"Content-Type": "application/json"}
            )
            response = connection.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.getheader("Cache-Control"), "no-store")
            self.assertEqual(json.loads(response.read())["draft_status"], "proposal_ready")
            connection.request(
                "POST", "/invocations", body=b"{}", headers={"Content-Type": "application/json"}
            )
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
