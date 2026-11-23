"""Trusted deployment policy for the initial AgentCore coding-agent path."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Callable, Mapping

from .bedrock import BedrockCodingModel, ConverseClient

_REGION = re.compile(r"^[a-z]{2}(?:-[a-z]+){1,2}-[0-9]+$")
_MODEL_ID = re.compile(r"^[A-Za-z0-9._:/-]{1,256}$")
_BUDGETS = {
    "max_iterations": (1, 10, 3),
    "max_turns": (1, 32, 12),
    "max_tool_calls": (1, 128, 32),
    "max_tokens": (1, 4096, 2048),
    # InvokeAgentRuntime is a synchronous API with a 15-minute request limit.
    # Longer jobs need a separately designed async runtime-job path.
    "attempt_timeout_seconds": (1, 900, 600),
}


@dataclass(frozen=True)
class AgentCorePolicy:
    """Exact model/Region and bounded workflow settings from trusted config.

    Repository files, task bodies, and model output are never configuration
    sources. The model ID is checked against a deployment-owned allowlist.
    """

    model_id: str
    region: str
    max_iterations: int = 3
    max_turns: int = 12
    max_tool_calls: int = 32
    max_tokens: int = 2048
    attempt_timeout_seconds: int = 600
    _validated: bool = field(default=False, init=False, repr=False, compare=False)

    @classmethod
    def from_deployment(cls, values: Mapping[str, str]) -> "AgentCorePolicy":
        if not isinstance(values, Mapping):
            raise ValueError("invalid AgentCore deployment configuration")
        model_value = values.get("HAKO_CODING_AGENT_MODEL_ID", "")
        region_value = values.get("HAKO_CODING_AGENT_BEDROCK_REGION", "")
        if not isinstance(model_value, str) or not isinstance(region_value, str):
            raise ValueError("invalid AgentCore deployment configuration")
        model_id = model_value.strip()
        region = region_value.strip()
        raw_allowlist = values.get("HAKO_CODING_AGENT_ALLOWED_MODEL_IDS", "")
        raw_region_allowlist = values.get("HAKO_CODING_AGENT_ALLOWED_BEDROCK_REGIONS", "")
        if not isinstance(raw_allowlist, str):
            raise ValueError("invalid AgentCore model allowlist")
        if not isinstance(raw_region_allowlist, str):
            raise ValueError("invalid AgentCore Region allowlist")
        try:
            allowed = json.loads(raw_allowlist)
        except (TypeError, json.JSONDecodeError) as exc:
            raise ValueError("invalid AgentCore model allowlist") from exc
        try:
            allowed_regions = json.loads(raw_region_allowlist)
        except json.JSONDecodeError as exc:
            raise ValueError("invalid AgentCore Region allowlist") from exc
        if (
            not isinstance(allowed, list)
            or not allowed
            or len(allowed) > 50
            or any(not isinstance(item, str) or not _MODEL_ID.fullmatch(item) for item in allowed)
            or len(set(allowed)) != len(allowed)
            or model_id not in allowed
            or not isinstance(allowed_regions, list)
            or not allowed_regions
            or len(allowed_regions) > 50
            or any(
                not isinstance(item, str) or not _REGION.fullmatch(item) for item in allowed_regions
            )
            or len(set(allowed_regions)) != len(allowed_regions)
            or region not in allowed_regions
            or not _MODEL_ID.fullmatch(model_id)
            or not _REGION.fullmatch(region)
        ):
            raise ValueError("AgentCore model or Region is not approved")
        budgets: dict[str, int] = {}
        for name, (minimum, maximum, default) in _BUDGETS.items():
            raw = values.get(f"HAKO_CODING_AGENT_{name.upper()}", "")
            if not isinstance(raw, str):
                raise ValueError(f"invalid AgentCore budget: {name}")
            if not raw:
                budgets[name] = default
                continue
            try:
                parsed = int(raw, 10)
            except (TypeError, ValueError) as exc:
                raise ValueError(f"invalid AgentCore budget: {name}") from exc
            if str(parsed) != raw or not minimum <= parsed <= maximum:
                raise ValueError(f"invalid AgentCore budget: {name}")
            budgets[name] = parsed
        policy = cls(model_id=model_id, region=region, **budgets)
        object.__setattr__(policy, "_validated", True)
        return policy

    def build_model(
        self, client: ConverseClient, *, deadline_check: Callable[[], None] | None = None
    ) -> BedrockCodingModel:
        """Construct the only supported initial harness from validated policy."""
        if not self._validated:
            raise ValueError("AgentCore policy was not validated from deployment config")
        return BedrockCodingModel(
            client,
            self.model_id,
            max_turns=self.max_turns,
            max_tool_calls=self.max_tool_calls,
            max_tokens=self.max_tokens,
            deadline_check=deadline_check,
        )
