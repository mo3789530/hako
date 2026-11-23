"""Optional AgentCore Runtime adapter; composition supplies trusted Hako adapters."""

import base64
from collections.abc import Callable, Mapping
from dataclasses import asdict
from typing import Any, Protocol

from .runtime import AttemptRunner, RuntimeConfig


class RuntimeApp(Protocol):
    def entrypoint(self, func: Callable[..., Any]) -> Callable[..., Any]: ...


def create_app(config: RuntimeConfig, runner: AttemptRunner) -> RuntimeApp:
    """Create an AgentCore app only when explicitly selected at deployment."""
    if config.backend != "agentcore":
        raise ValueError("AgentCore backend is not enabled")
    if runner.config != config:
        raise ValueError("runner configuration does not match deployment")
    try:
        from bedrock_agentcore import BedrockAgentCoreApp
    except ImportError as exc:
        raise RuntimeError("install hako-coding-agent[agentcore]") from exc

    app: RuntimeApp = BedrockAgentCoreApp()

    @app.entrypoint
    def handle(payload: Mapping[str, object]) -> dict[str, object]:
        if not isinstance(payload, Mapping):
            raise ValueError("invalid invocation payload")
        result = runner.run(payload)
        return {
            "attempt": asdict(result.attempt),
            "outcome": result.outcome.value,
            "iterations": result.iterations,
            "proposal": (
                {
                    "sha256": result.proposal.sha256,
                    "data_base64": base64.b64encode(result.proposal.data).decode("ascii"),
                }
                if result.proposal is not None
                else None
            ),
        }

    return app
