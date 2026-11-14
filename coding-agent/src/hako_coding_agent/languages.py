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
