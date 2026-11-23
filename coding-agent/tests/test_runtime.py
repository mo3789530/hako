import base64
import hashlib
import unittest
from unittest.mock import patch

from hako_coding_agent.agentcore import create_app
from hako_coding_agent.execution import Attempt, ExecutionResult
from hako_coding_agent.proposal import Proposal
from hako_coding_agent.runtime import (
    AttemptRunner,
    AuthorizedInput,
    AuthorizedMetadata,
    RepositoryInputResolver,
    RuntimeConfig,
)
from hako_coding_agent.workflow import Outcome


class RuntimeTest(unittest.TestCase):
    def test_deployment_config_is_explicit_and_rejects_invalid_values(self):
        self.assertEqual(RuntimeConfig.from_env({}), RuntimeConfig())
        config = RuntimeConfig.from_env(
            {
                "HAKO_AGENT_BACKEND": "agentcore",
                "HAKO_BEDROCK_PROMPT_CACHE": "true",
                "HAKO_AGENT_TENANT_ID": "tenant",
                "HAKO_AGENT_REPOSITORY_ID": "42",
            }
        )
        self.assertEqual(config.backend, "agentcore")
        self.assertTrue(config.bedrock_model(object(), "model").prompt_cache)
        for env in ({"HAKO_AGENT_BACKEND": "unknown"}, {"HAKO_BEDROCK_PROMPT_CACHE": "yes"}):
            with self.subTest(env=env), self.assertRaises(ValueError):
                RuntimeConfig.from_env(env)
        self.assertTrue(RuntimeConfig.from_env({"HAKO_AGENT_OBSERVABILITY": "true"}).observability)
        with self.assertRaises(ValueError):
            RuntimeConfig.from_env({"HAKO_AGENT_OBSERVABILITY": "yes"})
        for env in (
            {"HAKO_AGENT_BACKEND": "agentcore"},
            {
                "HAKO_AGENT_BACKEND": "agentcore",
                "HAKO_AGENT_TENANT_ID": "tenant",
                "HAKO_AGENT_REPOSITORY_ID": "0",
            },
        ):
            with self.subTest(env=env), self.assertRaises(ValueError):
                RuntimeConfig.from_env(env)

    def test_agentcore_app_requires_explicit_enablement(self):
        with self.assertRaisesRegex(ValueError, "not enabled"):
            create_app(RuntimeConfig(), object())

    def test_agentcore_entrypoint_delegates_and_encodes_proposal(self):
        class FakeApp:
            def entrypoint(self, func):
                self.handler = func
                return func

        class Runner:
            def run(self, payload):
                self.payload = payload
                data = b"proposal"
                return ExecutionResult(
                    Attempt("tenant", "run", "attempt", "plane", 1, 42, "a" * 40, "b" * 64),
                    Outcome.VERIFIED,
                    1,
                    Proposal(data, hashlib.sha256(data).hexdigest()),
                )

        config = RuntimeConfig(backend="agentcore", tenant_id="tenant", repository_id=42)
        runner = Runner()
        runner.config = config
        with patch("bedrock_agentcore.BedrockAgentCoreApp", FakeApp):
            app = create_app(config, runner)
        response = app.handler({"request": "opaque"})
        self.assertEqual(runner.payload, {"request": "opaque"})
        self.assertEqual(response["attempt"]["tenant_id"], "tenant")
        self.assertEqual(response["attempt"]["repository_id"], 42)
        self.assertEqual(base64.b64decode(response["proposal"]["data_base64"]), b"proposal")
        with self.assertRaises(ValueError):
            app.handler([])

    def test_runner_resolves_before_execution(self):
        class Resolver:
            def resolve(self, payload):
                raise PermissionError("denied")

        runner = AttemptRunner(Resolver(), object(), object())
        with patch("hako_coding_agent.runtime.execute") as execute:
            with self.assertRaises(PermissionError):
                runner.run({"attempt": "untrusted"})
            execute.assert_not_called()

    def test_agentcore_runner_rejects_other_tenants_and_repositories_before_execution(self):
        config = RuntimeConfig(backend="agentcore", tenant_id="tenant", repository_id=42)

        class Resolver:
            def __init__(self, attempt):
                self.attempt = attempt

            def resolve(self, payload):
                return AuthorizedInput(self.attempt, b"source", "task")

        for tenant, repo in (("other", 42), ("tenant", 43)):
            attempt = Attempt(tenant, "run", "attempt", "plane", 1, repo, "a" * 40, "b" * 64)
            runner = AttemptRunner(Resolver(attempt), object(), object(), config=config)
            with (
                self.subTest(tenant=tenant, repo=repo),
                patch("hako_coding_agent.runtime.execute") as execute,
            ):
                with self.assertRaises(PermissionError):
                    runner.run({})
                execute.assert_not_called()

    def test_agentcore_rejects_mismatched_runner_configuration(self):
        config = RuntimeConfig(backend="agentcore", tenant_id="tenant", repository_id=42)
        runner = AttemptRunner(object(), object(), object())
        with self.assertRaisesRegex(ValueError, "configuration does not match"):
            create_app(config, runner)

    def test_agentcore_accepts_only_resolved_scope_even_if_payload_claims_another(self):
        config = RuntimeConfig(backend="agentcore", tenant_id="tenant", repository_id=42)
        attempt = Attempt("tenant", "run", "attempt", "plane", 1, 42, "a" * 40, "b" * 64)

        class Resolver:
            def resolve(self, payload):
                return AuthorizedInput(attempt, b"source", "task")

        runner = AttemptRunner(Resolver(), object(), object(), config=config)
        expected = ExecutionResult(attempt, Outcome.UNVERIFIED, 1, None)
        with patch("hako_coding_agent.runtime.execute", return_value=expected) as execute:
            self.assertEqual(runner.run({"tenant_id": "other", "repository_id": 99}), expected)
            self.assertEqual(execute.call_args.args[0], attempt)

    def test_repository_resolver_checks_trusted_metadata_before_source_access(self):
        config = RuntimeConfig(backend="agentcore", tenant_id="tenant", repository_id=42)
        attempt = Attempt("tenant", "run", "attempt", "plane", 1, 42, "a" * 40, "b" * 64)

        class Metadata:
            def __init__(self, value):
                self.value = value

            def resolve(self, invocation_id):
                return AuthorizedMetadata(self.value, "task")

        class Source:
            def __init__(self):
                self.calls = []

            def fetch(self, value):
                self.calls.append(value)
                return b"source"

        source = Source()
        resolver = RepositoryInputResolver(config, Metadata(attempt), source)
        result = resolver.resolve({"invocation_id": "a" * 16})
        self.assertEqual(result.attempt, attempt)
        self.assertEqual(source.calls, [attempt])
        for foreign in (
            Attempt("other", "run", "attempt", "plane", 1, 42, "a" * 40, "b" * 64),
            Attempt("tenant", "run", "attempt", "plane", 1, 43, "a" * 40, "b" * 64),
        ):
            source.calls.clear()
            with self.subTest(foreign=foreign), self.assertRaises(PermissionError):
                RepositoryInputResolver(config, Metadata(foreign), source).resolve(
                    {"invocation_id": "a" * 16}
                )
            self.assertEqual(source.calls, [])
        for payload in (
            {},
            {"invocation_id": "short"},
            {"invocation_id": "a" * 16, "repository_id": 42},
        ):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                resolver.resolve(payload)

    def test_observability_reports_failure_without_payload(self):
        class Resolver:
            def resolve(self, payload):
                raise PermissionError("secret details")

        runner = AttemptRunner(
            Resolver(), object(), object(), config=RuntimeConfig(observability=True)
        )
        with self.assertLogs("hako_coding_agent.runtime", level="INFO") as logs:
            with self.assertRaises(PermissionError):
                runner.run({"task": "private task"})
        self.assertIn("agent attempt failed", logs.output[0])
        self.assertNotIn("private task", logs.output[0])
        self.assertNotIn("secret details", logs.output[0])
