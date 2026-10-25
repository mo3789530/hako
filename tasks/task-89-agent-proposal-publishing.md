# Task 89: Brokered Native Agent Branch and Pull Request Publishing

Status: Not started. Priority: P3. Depends on Tasks 39, 40, 53, 57, and 59.

## Goal

Publish a successful Native Agent proposal to one policy-approved branch and,
when separately allowed, open one pull request in the same Tenant-bound GitHub
Repository. The proposal remains untrusted. Hako must never merge or deploy it.

## Trust boundary

- The Guest/MicroVM never receives a GitHub write token or AWS credential.
- The Resource Plane persists the bounded `agent_changes` archive under the
  immutable Run/attempt identity and requests publication only after the Guest
  and MicroVM cleanup path reaches its required terminal checkpoint.
- A trusted broker re-resolves the current successful Run, latest attempt,
  Tenant/App/Installation/Repository pin, active Factory opt-in, current write
  policy, archive metadata/digest, and required passing Check results before
  minting a short-lived installation token.
- The installation token is restricted to the exact Repository and only
  `contents: write` plus `pull_requests: write`. It is held only in the broker
  process and is never returned to the Worker, Guest, API response, logs, or
  artifacts.
- The publisher creates a new policy-prefixed branch from the immutable base
  commit and opens a same-Repository PR against the current default branch.
  Existing branch contents are never overwritten. Merge and deployment APIs
  are not implemented by this path.

## Work

1. Define a durable, idempotent submission identity keyed by Tenant, Run,
   attempt, proposal digest, and pinned App/Repository generation. Model retry,
   lost GitHub responses, credential rotation, Factory disablement, and stale
   base branch without creating a second PR or changing a different branch.
2. Add a Control Plane resolver that reads the authoritative terminal Run,
   current attempt, successful Agent result, `agent_changes` artifact metadata,
   per-Repository branch/PR policy, required passing Hako Checks, current
   default branch, and immutable Tenant App identity. Reject missing or stale
   evidence before any credential request.
3. Add a broker-only endpoint and caller allowlist. Revalidate all pins before
   and after GitHub calls. Mint an exact-repository installation token with
   only Contents and Pull Requests write permissions; verify GitHub's returned
   repository and permission scope.
4. Add a bounded GitHub publisher that validates every archive entry before
   writes, checks the base ref against the pinned SHA, creates blobs/tree/commit
   and the new branch, and creates or recovers the one matching PR. It must
   reject forks, arbitrary base refs, existing unrelated branches, policy
   mismatch, failed required checks, and replay with a different digest.
5. Persist only bounded submission state and safe PR metadata. Do not persist
   the token, raw GitHub payloads, or archive contents in DSQL.
6. Add local fake-provider and PostgreSQL tests for success, duplicate request,
   lost response recovery, duplicate event, stale attempt/base/App generation,
   Factory disablement, cross-Tenant/Repository, foreign branch, failed Check,
   missing PR permission, token over-scope, API failure, cancellation, and
   cleanup-before-publish ordering.
7. Add API/CLI status and retry UX with no token or artifact reference exposure.
   Keep scheduling and publication default-off. Document coexistence with
   GitHub Actions and the human-review-only contract.

## Acceptance criteria

- No Guest/MicroVM process, client, log, Check output, or Artifact receives a
  GitHub write token.
- A duplicate or retried submission creates at most one branch and one PR;
  mismatched identity/digest is rejected without changing GitHub state.
- Only a new branch under the configured prefix and a same-Repository PR into
  the currently pinned default branch can be created, and only after required
  checks pass.
- Merge and deployment operations are impossible through the publisher.
- Focused unit, fake GitHub HTTP, and PostgreSQL integration tests pass with
  synthetic credentials. No live GitHub request is required for local
  acceptance.
- Real App permission changes, Secret retrieval, AWS deployment, and feature
  activation remain separately approved operational gates.

## Non-goals

- Automatically merge, deploy, or close a PR.
- Accept a PR from a fork or write to a user-selected branch.
- Reuse GitHub Actions or Claude Code Action credentials.
- Enable Agent scheduling before Task 40/53 MicroVM image, egress, and resource
  enforcement gates pass.
