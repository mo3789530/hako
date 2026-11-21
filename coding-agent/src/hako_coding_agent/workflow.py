"""Bounded coding workflow with explicit verification and review boundaries.

Adapters are deliberately injected: this module does not execute repository
commands, call a model, publish artifacts or contact AWS by itself.
"""

from dataclasses import dataclass
from enum import Enum
import re
from typing import Protocol


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
    def verify(self) -> Verification: ...
    def review(self) -> Review: ...


def run(steps: Steps, *, max_iterations: int = 3) -> Result:
    """Run plan/code/verify/review with a bounded number of rework cycles.

    A review on a different tree invalidates the measurement. An unmeasured
    tree is never reported verified. Errors from adapters propagate to the
    caller, which must record a failed run and perform cleanup.
    """
    if isinstance(max_iterations, bool) or not isinstance(max_iterations, int) or not 1 <= max_iterations <= 10:
        raise ValueError("max_iterations must be between 1 and 10")
    for iteration in range(1, max_iterations + 1):
        steps.plan(iteration)
        steps.code(iteration)
        verification = steps.verify()
        if not isinstance(verification, Verification) or not isinstance(verification.tree_digest, str) or not re.fullmatch(r"[0-9a-f]{64}", verification.tree_digest):
            raise ValueError("verification must identify the measured tree")
        if verification.exit_code is not None and (isinstance(verification.exit_code, bool) or not isinstance(verification.exit_code, int)):
            raise ValueError("invalid verification exit code")
        review = steps.review()
        if not isinstance(review, Review) or not isinstance(review.tree_digest, str) or not re.fullmatch(r"[0-9a-f]{64}", review.tree_digest) or not isinstance(review.approved, bool):
            raise ValueError("review must identify the reviewed tree")
        if verification.tree_digest != review.tree_digest:
            if iteration == max_iterations:
                return Result(Outcome.UNVERIFIED, iteration, review.tree_digest)
            continue
        if verification.exit_code is None:
            return Result(Outcome.UNVERIFIED, iteration, review.tree_digest)
        if verification.exit_code == 0 and review.approved:
            return Result(Outcome.VERIFIED, iteration, review.tree_digest)
        if iteration == max_iterations:
            return Result(Outcome.FAILED, iteration, review.tree_digest)
    raise AssertionError("unreachable")
