"""Local composition for one pinned Hako coding-agent attempt.

Identity here is binding metadata, not authentication. A trusted caller must
authorize every value and fence execution and result delivery independently.
"""

import re
from dataclasses import dataclass
from typing import Callable

from .agent import CodingModel, ProposalDraft, Verifier, propose_changes, run_agent
from .bedrock import ConverseClient
from .policy import AgentCorePolicy
from .proposal import Proposal
from .source import load_source
from .workflow import Outcome

_ID = re.compile(r"[A-Za-z0-9_-]{1,128}\Z")
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_COMMIT = re.compile(r"[0-9a-f]{40}\Z")


@dataclass(frozen=True)
class Attempt:
    tenant_id: str
    run_id: str
    attempt_id: str
    resource_plane_id: str
    generation: int
    repository_id: int
    commit_sha: str
    source_sha256: str

    def validate(self) -> None:
        if (any(not isinstance(value, str) or not _ID.fullmatch(value) for value in
                (self.tenant_id, self.run_id, self.attempt_id, self.resource_plane_id)) or
                type(self.generation) is not int or self.generation < 1 or
                type(self.repository_id) is not int or self.repository_id < 1 or
                not isinstance(self.commit_sha, str) or not _COMMIT.fullmatch(self.commit_sha) or
                not isinstance(self.source_sha256, str) or not _HASH.fullmatch(self.source_sha256)):
            raise ValueError("invalid attempt identity")


@dataclass(frozen=True)
class ExecutionResult:
    attempt: Attempt
    outcome: Outcome
    iterations: int
    proposal: Proposal | None


@dataclass(frozen=True)
class ProposalExecutionResult:
    """Attempt-bound metadata around an untrusted model proposal draft."""

    attempt: Attempt
    draft: ProposalDraft


def execute(attempt: Attempt, source_tar: bytes, task: str, model: CodingModel,
            verifier: Verifier, *, max_iterations: int = 3) -> ExecutionResult:
    """Run once using injected adapters, returning an untrusted local result.

    Only a locally verified run can return proposal bytes. This does not satisfy
    Hako's independent verifier, cleanup, cancellation, or publication gates.
    """
    if not isinstance(attempt, Attempt):
        raise ValueError("invalid attempt identity")
    attempt.validate()
    workspace = load_source(source_tar, attempt.source_sha256)
    result = run_agent(task, workspace, model, verifier, max_iterations=max_iterations)
    proposal = result.proposal if result.workflow.outcome == Outcome.VERIFIED else None
    return ExecutionResult(attempt, result.workflow.outcome, result.workflow.iterations, proposal)


def execute_with_policy(attempt: Attempt, source_tar: bytes, task: str,
                        client: ConverseClient, verifier: Verifier,
                        policy: AgentCorePolicy) -> ExecutionResult:
    """Compose the workflow only after trusted model/budget policy validation.

    The caller must create ``client`` in ``policy.region``. This function keeps
    model ID and workflow budgets out of task, repository and invocation input.
    """
    if not isinstance(policy, AgentCorePolicy):
        raise ValueError("trusted AgentCore policy is required")
    return execute(attempt, source_tar, task, policy.build_model(client), verifier,
                   max_iterations=policy.max_iterations)


def propose_with_policy(attempt: Attempt, source_tar: bytes, task: str,
                        client: ConverseClient, policy: AgentCorePolicy,
                        *, deadline_check: Callable[[], None] | None = None) -> ProposalExecutionResult:
    """Create a bounded proposal draft for downstream Hako verification."""
    if not isinstance(attempt, Attempt) or not isinstance(policy, AgentCorePolicy):
        raise ValueError("trusted attempt and AgentCore policy are required")
    model = policy.build_model(client, deadline_check=deadline_check)
    attempt.validate()
    workspace = load_source(source_tar, attempt.source_sha256)
    draft = propose_changes(task, workspace, model, max_iterations=policy.max_iterations)
    return ProposalExecutionResult(attempt, draft)
