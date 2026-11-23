"""Identify source languages for model context without selecting commands.

Detection is advisory and does not authorize execution or establish that a
project can be built. Mixed-language repositories may yield multiple entries.
"""

from collections.abc import Iterable
from pathlib import PurePosixPath

_SUFFIXES = {
    ".py": "Python",
    ".java": "Java",
    ".go": "Go",
    ".rs": "Rust",
    ".ts": "TypeScript",
    ".tsx": "TypeScript",
    ".tf": "Terraform",
    ".tfvars": "Terraform",
}
_MARKERS = {
    "pyproject.toml": "Python",
    "requirements.txt": "Python",
    "pom.xml": "Java",
    "build.gradle": "Java",
    "build.gradle.kts": "Java",
    "go.mod": "Go",
    "cargo.toml": "Rust",
    "tsconfig.json": "TypeScript",
}

_PLAN_GUIDANCE = {
    "Terraform": "Inspect modules, providers, state boundaries and existing validation; plan tests "
    "for configuration changes. Do not apply or access live state.",
    "Go": "Inspect go.mod, package boundaries, interfaces and existing table-driven tests; "
    "plan targeted tests, formatting and vet checks.",
    "Rust": "Inspect Cargo manifests, workspace features, ownership and error handling; "
    "plan targeted tests, formatting and appropriate checks.",
    "TypeScript": "Inspect package manifests, lockfiles, tsconfig, module boundaries and "
    "existing test tooling; plan type checks and behavior tests.",
    "Python": "Inspect pyproject and dependency configuration, supported Python versions, "
    "typing and existing tests; plan targeted tests, lint and type checks.",
    "Java": "Inspect Maven or Gradle configuration, Java version, module boundaries and "
    "existing test framework; plan targeted tests and build checks.",
}


def plan_guidance(languages: tuple[str, ...]) -> str:
    """Give advisory language-specific investigation points for the plan."""
    return "\n".join(f"{language}: {_PLAN_GUIDANCE[language]}" for language in languages)


def detect_languages(paths: Iterable[str]) -> tuple[str, ...]:
    """Return stable names inferred from visible source paths and manifests."""
    languages = set()
    for path in paths:
        if not isinstance(path, str):
            raise ValueError("invalid source path")
        name = PurePosixPath(path).name.lower()
        language = _MARKERS.get(name) or _SUFFIXES.get(PurePosixPath(name).suffix)
        if language:
            languages.add(language)
    return tuple(sorted(languages))
