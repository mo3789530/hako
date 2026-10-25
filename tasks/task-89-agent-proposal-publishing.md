# Task 89: Brokered Native Agent Branch and Pull Request Publishing

Status: In Progress (proposal materialization, isolated verifier scheduling,
digest-bound Check persistence, publication evidence resolution and durable
idempotency state, safe status API/CLI, exact-scope Tenant App token minting,
fake-tested GitHub publisher, and a Control Plane-only mTLS Broker publication
route with Postgres resolver/recorder, bounded post-cleanup verifier retry, and
API/CLI queued response, and default-off exact S3 proposal read IAM contracts
are implemented locally. Broker deployment/wiring, end-to-end Broker
composition, and runtime activation remain open). Priority: P3.
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
- Migration `000183` records trusted Resource Plane cleanup confirmation. On a
  successful Agent result with a proposal and enabled Repository Factory, the
  Control Plane now creates a same-Resource-Plane untrusted Job, attempt,
  dispatch Outbox, and immutable proposal binding atomically. Its event is
  `pull_request` for the current default branch. Resource Plane execution
  metadata (schema v4) exposes only pinned Agent Run/attempt, artifact ID,
  proposal digest, and base commit; it does not expose storage references.
- The Lambda Resource Plane composition now includes an identity-derived S3
  proposal reader. It verifies the exact attempt/digest object key, object
  metadata, bounded archive size, and content SHA-256 before overlaying the
  proposal on the verified base checkout and staging the deterministic source.
  Migration `000184` stores each verifier pipeline result with proposal digest,
  base commit, and attempt. Initial queued and terminal/pipeline GitHub Check
  writes are suppressed for verifier Runs, avoiding a false result on the base
  SHA. PostgreSQL integration verifies atomic verifier dispatch and digest-bound
  checks without a GitHub Check record.
- Migration `000185` adds a durable publication record keyed by Tenant/Agent
  Run/attempt, with immutable proposal/verifier/App/Repository/default-branch/
  policy pins and a deterministic branch name. The store re-resolves current
  publication evidence in the reservation transaction, rejects changed pins,
  and records only monotonic `requested` → `branch_created` →
  `pull_request_created` transitions. Exact retries are idempotent; a changed
  branch commit or PR number conflicts. It stores no token, archive bytes, or
  artifact URI. PostgreSQL integration covers reservation retry, the branch
  and PR transitions, and conflicting branch commit. Broker and GitHub writes
  are not wired yet.
- An authenticated Tenant/Requester-scoped status endpoint and
  `hako workload publication show <tenant-id> <agent-run-id>` expose only
  publication state, branch, confirmed commit, PR number, and timestamps.
  They return no artifact URI, proposal digest, token, verifier internals, or
  raw GitHub payload. PostgreSQL integration checks requester/cross-tenant
  isolation and metadata redaction; API client and CLI tests cover the route
  and output. The record selected for display follows the latest Agent attempt
  generation.
- `githubapp.TokenClient.AgentProposalPublishToken` now provides the
  credential primitive for a Tenant-owned Agent Run only. It requires exact
  Tenant App integration/config/credential-generation pins and requests one
  Repository with only `contents:write` and `pull_requests:write`; GitHub's
  returned repository and permission set are checked, and its cache key is
  distinct from read-only and Checks grants. Synthetic-key unit tests verify
  the scope and reject cross-Tenant/non-Agent requests. The token is exposed
  only to the trusted Broker service and is never serialized in the response.
- `AgentProposalGitHubWriter` validates the proposal archive again, verifies
  that the current default branch still points at the pinned base commit,
  creates blobs/tree/commit under the deterministic branch, and opens a
  same-Repository PR only when policy allows. Existing branches are accepted
  only when the branch commit has the exact expected tree, base parent, and
  Run/attempt/proposal marker. Existing PRs are queried by same-repository head
  and accepted only for the pinned base. Lost branch-ref and PR responses are
  recovered by reading those deterministic identities; unrelated branches,
  forks, stale base refs, and malformed archives fail closed. Fake GitHub HTTP
  tests cover normal create, retry, lost responses, stale base, and unrelated
  branch rejection. No live GitHub or real Secret request was made.
- `POST /v1/agent-proposals/publish` is now an mTLS Control Plane-only route.
  The Postgres resolver re-reads the latest successful Agent attempt, cleanup
  confirmation, digest-bound verifier Checks, active Tenant-owned App pins,
  Repository policy/default branch, and idempotency record. It resolves only
  exact-SHA S3 identity and the immutable private-key version. The service
  mints the one-Repository write token inside the Broker, fences ref creation
  against re-resolved pins, and records confirmed branch/PR progress even when
  a later provider response fails. Unit and HTTP tests cover non-Control-Plane
  rejection, unknown request fields, no-store/safe responses, stale-pin
  rejection, and partial provider result recording. Broker composition is
  opt-in through `HAKO_GITHUB_BROKER_AGENT_PROPOSAL_BUCKET` and optional
  `HAKO_GITHUB_BROKER_AGENT_PROPOSAL_PREFIX`; no secret or GitHub request is
  made during startup.
- The Control Plane now offers requester/Owner/Admin-authorized
  `POST /v1/tenants/{tenant}/workload-runs/{run}/agent-publication/publish`.
  It selects the latest attempt from PostgreSQL, calls the Broker over TLS 1.3
  mTLS, and returns only safe branch/PR metadata with `no-store`. Repeating the
  same call retries the idempotent identity. The CLI command is
  `hako workload publication submit <tenant-id> <agent-run-id>`; the focused
  PostgreSQL API E2E verifies latest-attempt forwarding, requester ownership,
  Owner/Admin-compatible access rules, and cross-Tenant/requester denial.
- Failed verifier attempts can be retried at most twice after cleanup
  confirmation. Retry reuses the same verifier Run and immutable Agent
  proposal/base/App/Repository/Resource Plane pins, allocates a new attempt and
  generation, and creates its dispatch Outbox in one PostgreSQL transaction.
  The verification binding follows the new attempt; old digest-bound Check
  rows remain generation-bound and cannot satisfy the retry. Duplicate submit
  while verification is active is idempotent; cancelled, timed-out, or
  uncleaned attempts are not retryable. API and CLI accept
  `202 verification_queued` and do not call the Broker until verifier success.
  PostgreSQL integration covers two retries, active retry idempotency, Outbox
  uniqueness, immutable proposal binding, and exhaustion.
- Terraform now has paired opt-in IAM contracts: the Control Plane Broker role
  receives only `s3:GetObject` on `<bucket>/<artifact-prefix>/*/*/*/agent-changes/*.zip`
  and optional `kms:Decrypt` on exact artifact key ARNs; the managed Resource
  Plane artifact bucket grants that Broker principal only `s3:GetObject` on
  the same proposal path. Inputs default empty. Cross-account KMS key policy,
  bucket configuration for external buckets, and Broker deployment
  configuration remain operator/deployment work; Terraform tests do not apply
  policies or retrieve Secrets.
- A composed PostgreSQL Broker integration test now sends a Control Plane
  caller through the HTTP route, Postgres resolver, digest-bound evidence and
  active Tenant App lookup, synthetic archive/token adapters, fenced publisher,
  and Postgres recorder. It verifies safe `no-store` response metadata and the
  durable branch/PR state. All providers are synthetic; no external service is
  contacted.
- Still open: deployed Broker configuration and Lambda MicroVM execution
  verification. No AWS apply or production activation has been performed.
