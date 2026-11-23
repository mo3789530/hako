"""Strict HTTP entrypoint for the AgentCore Runtime coding-agent profile.

AgentCore authenticates the invoker at the service boundary. This module
validates Hako's typed payload but does not authenticate its attempt pins; the
caller must derive them from durable Hako state and revalidate results later.
"""

from __future__ import annotations

import base64
import binascii
import json
import os
import time
from dataclasses import asdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Mapping

from .bedrock import ConverseClient
from .execution import Attempt, propose_with_policy
from .policy import AgentCorePolicy
from .source import MAX_TAR_BYTES

MAX_REQUEST_BYTES = 56 << 20
MAX_RESPONSE_BYTES = 12 << 20
MAX_TASK_BYTES = 64 << 10
_ATTEMPT_FIELDS = {
    "tenant_id", "run_id", "attempt_id", "resource_plane_id", "generation",
    "repository_id", "commit_sha", "source_sha256",
}


class InvocationInputError(ValueError):
    """The invocation body is malformed or exceeds its Hako contract."""


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise InvocationInputError("duplicate JSON field")
        result[key] = value
    return result


def _reject_constant(_: str) -> None:
    raise InvocationInputError("invalid JSON number")


def parse_invocation(body: bytes) -> tuple[Attempt, bytes, str]:
    """Parse the one supported versioned Hako request; unknown keys fail."""
    if not isinstance(body, bytes) or not 1 <= len(body) <= MAX_REQUEST_BYTES:
        raise InvocationInputError("invocation body size is invalid")
    try:
        request = json.loads(body.decode("utf-8", "strict"), object_pairs_hook=_unique_object,
                             parse_constant=_reject_constant)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError) as exc:
        raise InvocationInputError("invocation body is not valid JSON") from exc
    if (not isinstance(request, dict) or set(request) != {
            "schema_version", "attempt", "task", "source_archive_base64"} or
            type(request["schema_version"]) is not int or request["schema_version"] != 1):
        raise InvocationInputError("unsupported Hako invocation schema")
    raw_attempt = request["attempt"]
    if not isinstance(raw_attempt, dict) or set(raw_attempt) != _ATTEMPT_FIELDS:
        raise InvocationInputError("Hako attempt identity is invalid")
    try:
        attempt = Attempt(**raw_attempt)
        attempt.validate()
    except (TypeError, ValueError) as exc:
        raise InvocationInputError("Hako attempt identity is invalid") from exc
    task = request["task"]
    if not isinstance(task, str) or not task.strip() or len(task.encode("utf-8")) > MAX_TASK_BYTES:
        raise InvocationInputError("Hako task text is invalid")
    archive_text = request["source_archive_base64"]
    max_encoded = 4 * ((MAX_TAR_BYTES + 2) // 3)
    if not isinstance(archive_text, str) or not 1 <= len(archive_text) <= max_encoded:
        raise InvocationInputError("Hako source archive is invalid")
    try:
        archive = base64.b64decode(archive_text, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise InvocationInputError("Hako source archive is invalid") from exc
    if not 1 <= len(archive) <= MAX_TAR_BYTES:
        raise InvocationInputError("Hako source archive exceeds its size limit")
    return attempt, archive, task


class AgentCoreRuntime:
    """Application logic separated from HTTP and AWS for fake-backed tests."""

    def __init__(self, policy: AgentCorePolicy, client: ConverseClient,
                 monotonic: Callable[[], float] = time.monotonic):
        if not isinstance(policy, AgentCorePolicy) or client is None or monotonic is None:
            raise ValueError("AgentCore runtime dependencies are required")
        # Fail at startup if the policy was directly constructed or invalid.
        policy.build_model(client)
        self.policy = policy
        self.client = client
        self.monotonic = monotonic

    def invoke(self, body: bytes) -> bytes:
        started = self.monotonic()
        attempt, source_archive, task = parse_invocation(body)
        def enforce_deadline() -> None:
            if self.monotonic() - started >= self.policy.attempt_timeout_seconds:
                raise TimeoutError("AgentCore proposal deadline exceeded")

        enforce_deadline()
        result = propose_with_policy(attempt, source_archive, task, self.client, self.policy,
                                     deadline_check=enforce_deadline)
        if self.monotonic() - started >= self.policy.attempt_timeout_seconds:
            raise TimeoutError("AgentCore proposal deadline exceeded")
        draft = result.draft
        response: dict[str, Any] = {
            "schema_version": 1,
            "attempt": asdict(result.attempt),
            "draft_status": "proposal_ready" if draft.proposal is not None else "no_proposal",
            "iterations": draft.iterations,
            "tree_digest": draft.tree_digest,
            "proposal": None,
        }
        if draft.proposal is not None:
            response["proposal"] = {
                "sha256": draft.proposal.sha256,
                "data_base64": base64.b64encode(draft.proposal.data).decode("ascii"),
            }
        encoded = json.dumps(response, sort_keys=True, separators=(",", ":")).encode("utf-8")
        if len(encoded) > MAX_RESPONSE_BYTES:
            raise ValueError("AgentCore proposal response exceeds its size limit")
        return encoded


def make_handler(application: AgentCoreRuntime) -> type[BaseHTTPRequestHandler]:
    if application is None:
        raise ValueError("AgentCore runtime application is required")

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self) -> None:
            if self.path != "/ping":
                self._send(404, b'{"error":"not_found"}')
                return
            self._send(200, b'{"status":"Healthy"}')

        def do_POST(self) -> None:
            if self.path != "/invocations":
                self._send(404, b'{"error":"not_found"}')
                return
            if self.headers.get("Transfer-Encoding"):
                self._send(400, b'{"error":"invalid_request"}')
                return
            content_type = self.headers.get_content_type()
            if content_type != "application/json":
                self._send(415, b'{"error":"unsupported_media_type"}')
                return
            lengths = self.headers.get_all("Content-Length", [])
            if len(lengths) != 1 or not lengths[0].isascii() or not lengths[0].isdecimal():
                self._send(411, b'{"error":"content_length_required"}')
                return
            length = int(lengths[0], 10)
            if length < 1 or length > MAX_REQUEST_BYTES:
                self._send(413, b'{"error":"request_too_large"}')
                return
            body = self.rfile.read(length)
            if len(body) != length:
                self._send(400, b'{"error":"incomplete_request"}')
                return
            try:
                response = application.invoke(body)
            except InvocationInputError:
                self._send(400, b'{"error":"invalid_invocation"}')
                return
            except TimeoutError:
                self._send(504, b'{"error":"invocation_timeout"}')
                return
            except Exception:
                # Do not expose model, source, credential, or provider details.
                self._send(500, b'{"error":"invocation_failed"}')
                return
            self._send(200, response)

        def log_message(self, _format: str, *_args: Any) -> None:
            # Request metadata and model content stay out of stdout/stderr.
            return

        def _send(self, status: int, body: bytes) -> None:
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass

    return Handler


def create_bedrock_client(policy: AgentCorePolicy) -> ConverseClient:
    """Create a regional Bedrock client with bounded per-request transport."""
    if not isinstance(policy, AgentCorePolicy):
        raise ValueError("validated AgentCore policy is required")
    policy.build_model(_ClientValidationOnly())
    try:
        import boto3
        from botocore.config import Config
    except ImportError as exc:
        raise RuntimeError("install the coding-agent runtime extra") from exc
    return boto3.client("bedrock-runtime", region_name=policy.region,
                        config=Config(connect_timeout=5, read_timeout=60,
                                      retries={"max_attempts": 1, "mode": "standard"}))


class _ClientValidationOnly:
    """Satisfy the adapter's non-None constructor check without AWS access."""

    def converse(self, **_kwargs: Any) -> Mapping[str, Any]:
        raise RuntimeError("validation-only client cannot invoke Bedrock")


def main() -> None:
    policy = AgentCorePolicy.from_deployment(os.environ)
    client = create_bedrock_client(policy)
    server = ThreadingHTTPServer(("0.0.0.0", 8080), make_handler(AgentCoreRuntime(policy, client)))
    server.daemon_threads = True
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
