import json
import unittest

from hako_coding_agent.policy import AgentCorePolicy


class AgentCorePolicyTests(unittest.TestCase):
    def setUp(self):
        self.values = {
            "HAKO_CODING_AGENT_MODEL_ID": "us.anthropic.claude-test-v1",
            "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '["us.anthropic.claude-test-v1"]',
            "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1"]',
            "HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1",
        }

    def test_uses_exact_deployment_model_and_bounded_defaults(self):
        policy = AgentCorePolicy.from_deployment(self.values)
        self.assertEqual(policy.model_id, "us.anthropic.claude-test-v1")
        self.assertEqual(policy.region, "ap-northeast-1")
        self.assertEqual(
            (
                policy.max_iterations,
                policy.max_turns,
                policy.max_tool_calls,
                policy.max_tokens,
                policy.attempt_timeout_seconds,
            ),
            (3, 12, 32, 2048, 600),
        )

    def test_allows_explicit_budgets_within_hard_ceilings(self):
        policy = AgentCorePolicy.from_deployment(
            {
                **self.values,
                "HAKO_CODING_AGENT_MAX_ITERATIONS": "4",
                "HAKO_CODING_AGENT_MAX_TURNS": "16",
                "HAKO_CODING_AGENT_MAX_TOOL_CALLS": "64",
                "HAKO_CODING_AGENT_MAX_TOKENS": "3072",
                "HAKO_CODING_AGENT_ATTEMPT_TIMEOUT_SECONDS": "850",
            }
        )
        self.assertEqual(
            (
                policy.max_iterations,
                policy.max_turns,
                policy.max_tool_calls,
                policy.max_tokens,
                policy.attempt_timeout_seconds,
            ),
            (4, 16, 64, 3072, 850),
        )

    def test_builds_model_with_only_validated_model_and_budgets(self):
        policy = AgentCorePolicy.from_deployment(
            {
                **self.values,
                "HAKO_CODING_AGENT_MAX_TURNS": "7",
                "HAKO_CODING_AGENT_MAX_TOOL_CALLS": "9",
                "HAKO_CODING_AGENT_MAX_TOKENS": "1024",
            }
        )
        model = policy.build_model(object())
        self.assertEqual(model.model_id, policy.model_id)
        self.assertEqual((model.max_turns, model.max_tool_calls, model.max_tokens), (7, 9, 1024))

    def test_rejects_unapproved_model_and_invalid_regions(self):
        for changes in (
            {"HAKO_CODING_AGENT_MODEL_ID": "repository-selected-model"},
            {"HAKO_CODING_AGENT_BEDROCK_REGION": "https://example.test"},
            {"HAKO_CODING_AGENT_BEDROCK_REGION": "ap-northeast-1:other"},
            {"HAKO_CODING_AGENT_BEDROCK_REGION": "us-east-1"},
        ):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                AgentCorePolicy.from_deployment({**self.values, **changes})

    def test_rejects_bad_allowlist_and_budget_overrides(self):
        for changes in (
            {"HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": '"us.anthropic.claude-test-v1"'},
            {
                "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": json.dumps(
                    ["us.anthropic.claude-test-v1"] * 2
                )
            },
            {"HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": '["ap-northeast-1", "ap-northeast-1"]'},
            {"HAKO_CODING_AGENT_MAX_ITERATIONS": "11"},
            {"HAKO_CODING_AGENT_MAX_TOKENS": "4097"},
            {"HAKO_CODING_AGENT_MAX_TURNS": "01"},
            {"HAKO_CODING_AGENT_MAX_TOOL_CALLS": "0"},
            {"HAKO_CODING_AGENT_ATTEMPT_TIMEOUT_SECONDS": "901"},
        ):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                AgentCorePolicy.from_deployment({**self.values, **changes})

    def test_rejects_missing_or_malformed_trusted_configuration(self):
        for values in (
            {},
            {**self.values, "HAKO_CODING_AGENT_ALLOWED_MODEL_IDS": "not-json"},
            {**self.values, "HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS": "not-json"},
            {**self.values, "HAKO_CODING_AGENT_MODEL_ID": 7},
        ):
            with self.subTest(values=values), self.assertRaises(ValueError):
                AgentCorePolicy.from_deployment(values)

    def test_direct_construction_does_not_create_validated_policy(self):
        with self.assertRaises(ValueError):
            AgentCorePolicy("unapproved-model", "ap-northeast-1").build_model(object())


if __name__ == "__main__":
    unittest.main()
