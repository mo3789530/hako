# Task 93: Coding Agent Source and Proposal Contract

Status: Planned. Priority: P3. Parent: Task 90. Depends on Tasks 38, 53, 87 and 91.

## Goal

Decide and implement how an authorized attempt receives an immutable source
snapshot and returns a complete, safely validated change proposal.

## Decisions and work

1. Specify trusted delivery from pinned Tenant/Repository/commit/attempt to
   the selected workspace, digest and provenance validation, expiry and size
   limits. Define handling of symlinks, binary and large files, permissions,
   submodules, ignored files and unsupported source without silent omission.
2. Decide a versioned proposal representation for adds, edits, deletions,
   renames and executable-bit changes. Determine compatibility with the
   existing regular-file-only `agent_changes` ZIP and Task 89 broker and
   verifier. Unsupported operations must fail explicitly, never disappear.
3. Define allowed repository/path policy, validation order, collision handling,
   archive bounds and how the exact proposal digest and base SHA bind to the
   verifier. A caller-supplied attempt ID or digest is not authentication.
4. Implement source/proposal adapters and local cross-language tests using
   synthetic archives, including traversal, links, duplicate entries, stale
   attempt, changed digest, modes, deletion and protected paths.

## Acceptance criteria

- Round-trip tests prove every supported change appears in the exact tree
  independently verified and later submitted; unsupported changes fail closed.
- Cross-Tenant or wrong-commit source and artifact access are rejected.
- AWS storage, IAM, actual filesystem and cleanup validation remain open until
  exercised in a deployed environment.
