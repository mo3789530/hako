import hashlib
import io
import tarfile
import unittest
from dataclasses import replace

from hako_coding_agent.bedrock import BedrockCodingModel
from hako_coding_agent.execution import Attempt, execute
from hako_coding_agent.workflow import Outcome


def response(text):
    return {
        "stopReason": "end_turn",
        "output": {"message": {"role": "assistant", "content": [{"text": text}]}},
    }


class Client:
    def __init__(self):
        self.responses = iter(
            [
                response("Update the file"),
                {
                    "stopReason": "tool_use",
                    "output": {
                        "message": {
                            "role": "assistant",
                            "content": [
                                {
                                    "toolUse": {
                                        "toolUseId": "a",
                                        "name": "write_file",
                                        "input": {"path": "app.py", "content": "new\n"},
                                    }
                                }
                            ],
                        }
                    },
                },
                response("Done"),
                response("Tests updated"),
                response("PASS"),
            ]
        )
        self.calls = 0

    def converse(self, **kwargs):
        self.calls += 1
        return next(self.responses)


class Verifier:
    def __init__(self, result=0):
        self.result = result

    def verify(self, workspace):
        return self.result


def source():
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as tar:
        item = tarfile.TarInfo("app.py")
        item.size = 4
        tar.addfile(item, io.BytesIO(b"old\n"))
    return output.getvalue()


class ExecutionTest(unittest.TestCase):
    def setUp(self):
        self.source = source()
        self.attempt = Attempt(
            "tenant",
            "run",
            "attempt",
            "plane",
            1,
            42,
            "a" * 40,
            hashlib.sha256(self.source).hexdigest(),
        )

    def test_pinned_source_to_verified_proposal(self):
        client = Client()
        result = execute(
            self.attempt, self.source, "Update app", BedrockCodingModel(client, "model"), Verifier()
        )
        self.assertEqual(result.attempt, self.attempt)
        self.assertEqual(result.outcome, Outcome.VERIFIED)
        self.assertIsNotNone(result.proposal)
        self.assertEqual(result.proposal.sha256, hashlib.sha256(result.proposal.data).hexdigest())

    def test_bad_identity_or_source_rejected_before_model(self):
        for attempt in (
            replace(self.attempt, generation=0),
            replace(self.attempt, source_sha256="0" * 64),
            replace(self.attempt, repository_id=True),
        ):
            client = Client()
            with self.subTest(attempt=attempt), self.assertRaises(ValueError):
                execute(
                    attempt,
                    self.source,
                    "Update app",
                    BedrockCodingModel(client, "model"),
                    Verifier(),
                )
            self.assertEqual(client.calls, 0)

    def test_unverified_result_has_no_proposal(self):
        result = execute(
            self.attempt,
            self.source,
            "Update app",
            BedrockCodingModel(Client(), "model"),
            Verifier(None),
        )
        self.assertEqual(result.outcome, Outcome.UNVERIFIED)
        self.assertIsNone(result.proposal)


if __name__ == "__main__":
    unittest.main()
