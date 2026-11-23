"""Composition of the local coding workflow and bounded workspace tools."""

from collections.abc import Callable
from dataclasses import dataclass
from typing import Protocol

from .proposal import Proposal
from .workflow import Result, Review, Verification, run
from .workspace import Workspace


class CodingModel(Protocol):
    """The model adapter receives bounded workspace tools, not host paths."""

    def plan(self, task: str, workspace: Workspace, iteration: int) -> str: ...
    def code(self, task: str, plan: str, workspace: Workspace) -> None: ...
    def test(self, task: str, plan: str, workspace: Workspace) -> None: ...
    def review(self, task: str, workspace: Workspace) -> bool: ...


class Verifier(Protocol):
    def verify(self, workspace: Workspace) -> int | None: ...


@dataclass(frozen=True)
class AgentResult:
    workflow: Result
    proposal: Proposal | None


@dataclass(frozen=True)
class ProposalDraft:
    """Untrusted proposal draft for Hako's separate verifier."""

    iterations: int
    tree_digest: str
    proposal: Proposal | None


class _Steps:
    def __init__(self, task: str, workspace: Workspace, model: CodingModel, verifier: Verifier):
        self.task = task
        self.workspace = workspace
        self.model = model
        self.verifier = verifier
        self.current_plan = ""

    def plan(self, iteration: int) -> None:
        plan = self.model.plan(self.task, self.workspace, iteration)
        if not isinstance(plan, str) or not plan.strip() or len(plan.encode("utf-8")) > 32 << 10:
            raise ValueError("invalid model plan")
        self.current_plan = plan

    def code(self, iteration: int) -> None:
        self.model.code(self.task, self.current_plan, self.workspace)

    def test(self, iteration: int) -> None:
        self.model.test(self.task, self.current_plan, self.workspace)

    def verify(self) -> Verification:
        before = self.workspace.tree_digest()
        exit_code = self.verifier.verify(self.workspace)
        # A verifier that mutates its input did not measure the resulting tree.
        if self.workspace.tree_digest() != before:
            raise ValueError("verification changed the source tree")
        return Verification(exit_code, before)

    def review(self) -> Review:
        approved = self.model.review(self.task, self.workspace)
        return Review(approved, self.workspace.tree_digest())


def run_agent(
    task: str,
    workspace: Workspace,
    model: CodingModel,
    verifier: Verifier,
    *,
    max_iterations: int = 3,
    observe: Callable[[str, int, str, int], None] | None = None,
) -> AgentResult:
    """Run with injected adapters; results remain subject to Hako verification."""
    if not isinstance(task, str) or not task.strip() or len(task.encode("utf-8")) > 64 << 10:
        raise ValueError("invalid task")
    result = run(
        _Steps(task, workspace, model, verifier), max_iterations=max_iterations, observe=observe
    )
    # A success without a proposal is not evidence of a repository change.
    return AgentResult(result, workspace.proposal())


def propose_changes(
    task: str, workspace: Workspace, model: CodingModel, *, max_iterations: int = 3
) -> ProposalDraft:
    """Produce an untrusted draft without executing or claiming verification.

    The returned ZIP must still pass Hako's independent verifier and current
    attempt/digest checks before it can affect a Run or publication decision.
    """
    if (
        not isinstance(task, str)
        or not task.strip()
        or len(task.encode("utf-8")) > 64 << 10
        or not isinstance(workspace, Workspace)
        or model is None
    ):
        raise ValueError("invalid proposal request")
    if (
        isinstance(max_iterations, bool)
        or not isinstance(max_iterations, int)
        or not 1 <= max_iterations <= 10
    ):
        raise ValueError("max_iterations must be between 1 and 10")
    current_plan = ""
    for iteration in range(1, max_iterations + 1):
        plan = model.plan(task, workspace, iteration)
        if not isinstance(plan, str) or not plan.strip() or len(plan.encode("utf-8")) > 32 << 10:
            raise ValueError("invalid model plan")
        current_plan = plan
        model.code(task, current_plan, workspace)
        model.test(task, current_plan, workspace)
        before_review = workspace.tree_digest()
        approved = model.review(task, workspace)
        after_review = workspace.tree_digest()
        if not isinstance(approved, bool):
            raise ValueError("invalid model review")
        if before_review != after_review:
            continue
        proposal = workspace.proposal() if approved else None
        if proposal is not None:
            return ProposalDraft(iteration, after_review, proposal)
    return ProposalDraft(max_iterations, workspace.tree_digest(), None)
