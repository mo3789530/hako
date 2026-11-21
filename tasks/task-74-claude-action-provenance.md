# Task 74: Claude Action workflow provenance and eligible-job contract

Status: In Progress (a fail-closed verifier, repository-scoped GitHub REST Run/Job lookup, Owner/Admin policy API/CLI, and launch-time preflight are implemented locally. Production runtime and Task 76/77 end-to-end execution remain separate). Priority: P2. Parent: Task 73. Depends on: Tasks 51, 52.

## Goal

Treat the repository's explicit Claude Code Action workflow as an ordinary GitHub Actions job on Hako's ephemeral runner, and bind every launch to the exact authorized workflow/job identity. Hako must not rewrite or infer a workflow from an issue or PR comment.

## Work

- Define the allowed event, repository installation, workflow path/ref, commit SHA, workflow/job run IDs, runner labels and Tenant policy inputs.
- [x] Define a data-only allowlist contract and pure verifier. Forks and `pull_request_target`-style unapproved events fail closed; policy cannot specify commands, credentials, images, roles or network profiles.
- [x] Add Tenant App repository-scoped `actions:read` Installation token issuance with the runner opt-in rechecked before and after minting. No contents, write, administration, registration, or workflow-dispatch permission is included.
- [x] Add bounded fake-testable GitHub REST lookup for exact Run, attempt, Job and workflow path. Redirects, oversized responses, missing/ambiguous jobs, wrong repository/run/attempt, and malformed workflow metadata fail closed.
- [x] Persist the per-Tenant/repository allowlist through an Owner/Admin API and CLI, audit changes, and clear it transactionally when runner opt-in is disabled.
- [x] Resolve only a processed, current-generation webhook job and fetch authoritative run/job/workflow metadata before the launch starter consumes the claim. The service composition runs this before returning a launch command; no issue/comment content is interpreted.
- [x] Wire the persisted policy and exact queued identity verification before claim consumption. The starter rechecks the exact JSONB policy snapshot inside the claim transaction; changed/removed policy or a non-verified legacy starter is denied.
- [x] Document the user-authored workflow contract and Hako runner labels. Action revisions remain user-controlled and must be pinned by full commit SHA; Hako does not bundle or silently upgrade the Action.

## Acceptance criteria

- Fake GitHub tests reject wrong repository/workflow/ref/SHA/job ID and fork PR; launch service tests prove preflight runs before the starter and failures cannot consume a claim. PostgreSQL tests cover current-policy snapshot fencing.
- Duplicate queued deliveries create at most one run; retries return the same run identity.
- No issue/comment text can directly select arbitrary commands, credentials, workflow files or runner privilege.
- Workflow contract and unsupported cases are documented without changing existing GitHub Actions YAML semantics.
