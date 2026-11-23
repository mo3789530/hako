"""Deployment configuration and provider-neutral authorized attempt dispatch."""

import logging
import re
import time
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Protocol

from .agent import CodingModel, Verifier
from .bedrock import BedrockCodingModel, ConverseClient
from .execution import Attempt, ExecutionResult, execute


@dataclass(frozen=True)
class RuntimeConfig:
    backend: str = "local"
    prompt_cache: bool = False
    observability: bool = False
    tenant_id: str | None = None
    repository_id: int | None = None

    def __post_init__(self) -> None:
        if self.backend not in ("local", "agentcore"):
            raise ValueError("invalid agent runtime configuration")
        if self.backend == "agentcore" and (
            not isinstance(self.tenant_id, str)
            or not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", self.tenant_id)
            or type(self.repository_id) is not int
            or self.repository_id < 1
        ):
            raise ValueError("agentcore requires a tenant and repository scope")
        if self.backend == "local" and (
            self.tenant_id is not None or self.repository_id is not None
        ):
            raise ValueError("local backend cannot use agentcore repository scope")

    @classmethod
    def from_env(cls, env: Mapping[str, str]) -> "RuntimeConfig":
        backend = env.get("HAKO_AGENT_BACKEND", "local")
        cache = env.get("HAKO_BEDROCK_PROMPT_CACHE", "false")
        observability = env.get("HAKO_AGENT_OBSERVABILITY", "false")
        tenant_id = env.get("HAKO_AGENT_TENANT_ID")
        repository = env.get("HAKO_AGENT_REPOSITORY_ID")
        if (
            backend not in ("local", "agentcore")
            or cache not in ("true", "false")
            or observability not in ("true", "false")
        ):
            raise ValueError("invalid agent runtime configuration")
        if repository is not None and (
            not repository.isascii() or not repository.isdecimal() or len(repository) > 19
        ):
            raise ValueError("invalid agent runtime configuration")
        return cls(
            backend=backend,
            prompt_cache=cache == "true",
            observability=observability == "true",
            tenant_id=tenant_id,
            repository_id=int(repository) if repository else None,
        )

    def bedrock_model(self, client: ConverseClient, model_id: str) -> BedrockCodingModel:
        return BedrockCodingModel(client, model_id, prompt_cache=self.prompt_cache)


@dataclass(frozen=True)
class AuthorizedInput:
    attempt: Attempt
    source_tar: bytes
    task: str


class InputResolver(Protocol):
    """Resolve and authorize an opaque invocation using trusted Hako metadata."""

    def resolve(self, payload: Mapping[str, object]) -> AuthorizedInput: ...


@dataclass(frozen=True)
class AuthorizedMetadata:
    """Attempt and task returned by a trusted, authenticated control-plane adapter."""

    attempt: Attempt
    task: str


class MetadataProvider(Protocol):
    def resolve(self, invocation_id: str) -> AuthorizedMetadata: ...


class SourceProvider(Protocol):
    def fetch(self, attempt: Attempt) -> bytes: ...


class RepositoryInputResolver:
    """Resolve an opaque invocation through a trusted provider and enforce deployment scope.

    The metadata provider must authenticate requests and authorize the current
    attempt, including cancellation and lease checks. Invocation IDs are not
    credentials; this class does not supply those guarantees.
    """

    def __init__(self, config: RuntimeConfig, metadata: MetadataProvider, source: SourceProvider):
        if config.backend != "agentcore":
            raise ValueError("repository resolver requires agentcore scope")
        self.config = config
        self.metadata = metadata
        self.source = source

    def resolve(self, payload: Mapping[str, object]) -> AuthorizedInput:
        if set(payload) != {"invocation_id"}:
            raise ValueError("invalid invocation")
        invocation_id = payload["invocation_id"]
        if not isinstance(invocation_id, str) or not re.fullmatch(
            r"[A-Za-z0-9_-]{16,128}", invocation_id
        ):
            raise ValueError("invalid invocation")
        metadata = self.metadata.resolve(invocation_id)
        metadata.attempt.validate()
        if (
            metadata.attempt.tenant_id != self.config.tenant_id
            or metadata.attempt.repository_id != self.config.repository_id
        ):
            raise PermissionError("attempt is outside configured repository scope")
        return AuthorizedInput(metadata.attempt, self.source.fetch(metadata.attempt), metadata.task)


class AttemptRunner:
    def __init__(
        self,
        resolver: InputResolver,
        model: CodingModel,
        verifier: Verifier,
        *,
        config: RuntimeConfig = RuntimeConfig(),
    ):
        self.resolver = resolver
        self.model = model
        self.verifier = verifier
        self.config = config

    def run(self, payload: Mapping[str, object]) -> ExecutionResult:
        start = time.monotonic()

        def observe(stage: str, iteration: int, status: str, duration_ms: int) -> None:
            logging.getLogger(__name__).info(
                "agent stage=%s iteration=%d status=%s duration_ms=%d",
                stage,
                iteration,
                status,
                duration_ms,
            )

        try:
            resolved = self.resolver.resolve(payload)
            resolved.attempt.validate()
            if self.config.backend == "agentcore" and (
                resolved.attempt.tenant_id != self.config.tenant_id
                or resolved.attempt.repository_id != self.config.repository_id
            ):
                raise PermissionError("attempt is outside configured repository scope")
            result = execute(
                resolved.attempt,
                resolved.source_tar,
                resolved.task,
                self.model,
                self.verifier,
                observe=observe if self.config.observability else None,
            )
        except Exception:
            if self.config.observability:
                logging.getLogger(__name__).info(
                    "agent attempt failed duration_ms=%d", int((time.monotonic() - start) * 1000)
                )
            raise
        if self.config.observability:
            logging.getLogger(__name__).info(
                "agent attempt completed outcome=%s duration_ms=%d",
                result.outcome.value,
                int((time.monotonic() - start) * 1000),
            )
        return result
