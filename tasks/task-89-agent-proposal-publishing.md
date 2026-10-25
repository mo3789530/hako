# Task 89: Brokered Native Agent Branch and Pull Request Publishing

Status: In Progress (safe proposal-to-source materialization is implemented;
isolated verifier Workload scheduling, digest-bound Check persistence, broker
authorization, and GitHub publication are not yet implemented). Priority: P3.
Depends on Tasks 39, 40, 53, 57, and 59.

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

1. Materialize the bounded proposal onto its exact immutable base in a separate
   verifier Workload with no GitHub write token, model credential, or cloud
   credential. Run only the Repository's allowlisted, bounded Hako checks.
   Persist an immutable verification result bound to the proposal SHA-256, base
   commit, verifier Run/attempt, and each required Check name. A successful
   Agent Run or a Check on the unmodified base commit is not proof that the
   proposed tree passed.
2. Define a durable, idempotent submission identity keyed by Tenant, Agent Run,
   attempt, proposal digest, verifier Run/attempt, and pinned App/Repository
   generation. Model retry, lost GitHub responses, credential rotation,
   Factory disablement, and stale base branch without creating a second PR or
   changing a different branch.
3. Add a Control Plane resolver that reads the authoritative terminal Agent
   Run and cleanup confirmation, latest attempt, `agent_changes` artifact
   metadata, the digest-bound verifier result for every required Check,
   per-Repository branch/PR policy, current default branch, and immutable
   Tenant App identity. Reject missing, stale, or base-only evidence before any
   credential request.
4. Add a broker-only endpoint and caller allowlist. Revalidate all pins before
   and after GitHub calls. Mint an exact-repository installation token with
   only Contents and Pull Requests write permissions; verify GitHub's returned
   repository and permission scope.
5. Add a bounded GitHub publisher that validates every archive entry before
   writes, checks the base ref against the pinned SHA, creates blobs/tree/commit
   and the new branch, and creates or recovers the one matching PR. It must
   reject forks, arbitrary base refs, existing unrelated branches, policy
   mismatch, failed required checks, and replay with a different digest.
6. Persist only bounded submission state and safe PR metadata. Do not persist
   the token, raw GitHub payloads, or archive contents in DSQL.
7. Add local fake-provider and PostgreSQL tests for success, duplicate request,
   lost response recovery, duplicate event, stale attempt/base/App generation,
   Factory disablement, cross-Tenant/Repository, foreign branch, failed Check,
   base-only Check, changed proposal digest after verification, missing PR
   permission, token over-scope, API failure, cancellation, and
   cleanup-before-publish ordering.
8. Add API/CLI status and retry UX with no token or artifact reference exposure.
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

## Local implementation progress (2026-10-25)

- `workloadsource.ApplyAgentProposalArchive` verifies the immutable base source
  archive, validates the untrusted ZIP against path/type/secret/size bounds,
  applies only regular files in a private temporary checkout, and emits a new
  deterministic source archive. It returns both the proposal ZIP digest and
  base source digest so a future verifier Workload can bind its source identity
  to the exact proposal. The shared inspector uses the same validator. Unit
  tests cover deterministic output, changed/new files, wrong base digest,
  traversal, protected paths, secret content, symlinks, duplicate entries, and
  size limits. This is only a source materialization primitive: it does not run
  tests, create a verifier Run, or authorize a GitHub write.
- Migration `000181` and `BindAgentProposalVerification` persist an immutable
  relation from the exact successful, cleanup-confirmed Agent attempt and
  `agent_changes` artifact digest/base commit to a separate untrusted Job Run
  and attempt. The store verifies same-Tenant Repository/App-generation/base
  pins, latest attempts, live verifier state, and retry identity inside the
  transaction. A scoped getter exposes binding metadata without returning the
  artifact storage reference. The focused PostgreSQL integration test passes;
  scheduling, source-byte handoff/materialization, required Check evidence,
  and any publication path remain unimplemented.
