"""Bounded coding workflow with explicit verification and review boundaries.

Adapters are deliberately injected: this module does not execute repository
commands, call a model, publish artifacts or contact AWS by itself.
"""

import re
import time
from collections.abc import Callable
from dataclasses import dataclass
from enum import Enum
from typing import Literal, NotRequired, Protocol, TypedDict

from langgraph.graph import START, StateGraph


class Outcome(str, Enum):
    VERIFIED = "verified"
    UNVERIFIED = "unverified"
    FAILED = "failed"


@dataclass(frozen=True)
class Verification:
    # None means no verification ran, not a passing exit code.
    exit_code: int | None
    tree_digest: str


@dataclass(frozen=True)
class Review:
    approved: bool
    tree_digest: str


@dataclass(frozen=True)
class Result:
    outcome: Outcome
    iterations: int
    tree_digest: str


class Steps(Protocol):
    def plan(self, iteration: int) -> None: ...
    def code(self, iteration: int) -> None: ...
    def test(self, iteration: int) -> None: ...
    def verify(self) -> Verification: ...
    def review(self) -> Review: ...


class _State(TypedDict):
    iteration: int
    verification: NotRequired[Verification]
    review: NotRequired[Review]
    result: Result | None


def run(
    steps: Steps,
    *,
    max_iterations: int = 3,
    observe: Callable[[str, int, str, int], None] | None = None,
) -> Result:
    """Run plan/code/test/verify/review with a bounded number of rework cycles.

    A review on a different tree invalidates the measurement. An unmeasured
    tree is never reported verified. Errors from adapters propagate to the
    caller, which must record a failed run and perform cleanup.
    """
    if (
        isinstance(max_iterations, bool)
        or not isinstance(max_iterations, int)
        or not 1 <= max_iterations <= 10
    ):
        raise ValueError("max_iterations must be between 1 and 10")

    def invoke_stage(name: str, iteration: int, action: Callable[[], object]) -> object:
        start = time.monotonic()
        try:
            value = action()
        except Exception:
            if observe is not None:
                observe(name, iteration, "failed", int((time.monotonic() - start) * 1000))
            raise
        if observe is not None:
            observe(name, iteration, "completed", int((time.monotonic() - start) * 1000))
        return value

    def plan(state: _State) -> dict[str, int]:
        iteration = state["iteration"] + 1
        invoke_stage("plan", iteration, lambda: steps.plan(iteration))
        return {"iteration": iteration}

    def code(state: _State) -> dict[str, object]:
        invoke_stage("code", state["iteration"], lambda: steps.code(state["iteration"]))
        return {}

    def test(state: _State) -> dict[str, object]:
        invoke_stage("test", state["iteration"], lambda: steps.test(state["iteration"]))
        return {}

    def verify(state: _State) -> dict[str, Verification]:
        verification = invoke_stage("verify", state["iteration"], steps.verify)
        if (
            not isinstance(verification, Verification)
            or not isinstance(verification.tree_digest, str)
            or not re.fullmatch(r"[0-9a-f]{64}", verification.tree_digest)
        ):
            raise ValueError("verification must identify the measured tree")
        if verification.exit_code is not None and (
            isinstance(verification.exit_code, bool) or not isinstance(verification.exit_code, int)
        ):
            raise ValueError("invalid verification exit code")
        return {"verification": verification}

    def review(state: _State) -> dict[str, Review]:
        review = invoke_stage("review", state["iteration"], steps.review)
        if (
            not isinstance(review, Review)
            or not isinstance(review.tree_digest, str)
            or not re.fullmatch(r"[0-9a-f]{64}", review.tree_digest)
            or not isinstance(review.approved, bool)
        ):
            raise ValueError("review must identify the reviewed tree")
        return {"review": review}

    def decide(state: _State) -> dict[str, Result | None]:
        iteration = state["iteration"]
        verification = state["verification"]
        review = state["review"]
        if verification.tree_digest != review.tree_digest:
            if iteration == max_iterations:
                return {"result": Result(Outcome.UNVERIFIED, iteration, review.tree_digest)}
            return {"result": None}
        if verification.exit_code is None:
            return {"result": Result(Outcome.UNVERIFIED, iteration, review.tree_digest)}
        if verification.exit_code == 0 and review.approved:
            return {"result": Result(Outcome.VERIFIED, iteration, review.tree_digest)}
        if iteration == max_iterations:
            return {"result": Result(Outcome.FAILED, iteration, review.tree_digest)}
        return {"result": None}

    def route(state: _State) -> Literal["plan", "__end__"]:
        return "__end__" if state["result"] is not None else "plan"

    graph = StateGraph(_State)
    graph.add_node("plan", plan)
    graph.add_node("code", code)
    graph.add_node("test", test)
    graph.add_node("verify", verify)
    graph.add_node("review", review)
    graph.add_node("decide", decide)
    graph.add_edge(START, "plan")
    graph.add_edge("plan", "code")
    graph.add_edge("code", "test")
    graph.add_edge("test", "verify")
    graph.add_edge("verify", "review")
    graph.add_edge("review", "decide")
    graph.add_conditional_edges("decide", route)
    final = graph.compile().invoke({"iteration": 0, "result": None}, {"recursion_limit": 61})
    result = final["result"]
    if not isinstance(result, Result):
        raise AssertionError("workflow ended without a result")
    return result
