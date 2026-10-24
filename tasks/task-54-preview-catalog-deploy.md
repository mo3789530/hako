# Task 54: Preview Workspaces, Software Catalog, and OIDC Deploy Policy

Status: In Progress. The Tenant-scoped Software Catalog metadata API/CLI and
PostgreSQL RBAC/reference integration coverage are implemented. OIDC deploy
policy API/CLI and fixed-GitHub-issuer RS256/JWKS claim verification are now
implemented locally; no STS exchange or deployment execution is enabled.
Tenant-scoped Preview opt-in and transactional PR desired-state tracking are
implemented locally, including stale GitHub `updated_at` fencing, fixed
Tenant-quota-based expiry metadata, and a bounded idempotent expiry-to-cleanup
transition. A default-off scheduled Lambda now invokes this metadata sweep;
production IAM wiring and enabled invocation remain unverified. Preview
MicroVM allocation, Gateway routing, and physical cleanup execution remain
incomplete; cleanup retry metadata and attempt-fenced leases are implemented.
The dormant approval verifier checks
the live Actions run attempt against signed repository, owner, ref, SHA,
workflow and event claims, checks pending deployments and approval history,
and requires Environment reviewers with self-review disabled. Priority: P3.

## Goal

Add optional repository-level preview environments and service metadata/deploy policy after the Workspace Runtime and Gateway are production-ready.

## Work

- Create one isolated Preview Run/Workspace per authorized PR commit; expose only through Gateway with expiring Tenant-scoped routing; tear down on PR close/merge/timeout.
- Add a Tenant-scoped Software Catalog mapping service, owner/team, repository, runtime/language, dependencies, and production URL without duplicating secret data.
- Add GitHub OIDC cloud identity only for trusted release/deploy Workloads; constrain cloud trust by Repository, ref/environment, and verified workflow claims. Local dormant policy verification is implemented; STS exchange and cloud identity are not.
- Require explicit human approval and policy checks for production; untrusted forks and agents receive no deploy role. Local verification checks the exact live run and approval state but does not authorize deployment.

## Software Catalog metadata slice (2026-10-24)

- Added Tenant-scoped `GET /v1/tenants/{tenant_id}/software-catalog` and
  `PUT /v1/tenants/{tenant_id}/software-catalog/{service_id}`. Tenant members
  may read; only Owners/Admins may write. Writes require a Repository already
  registered to that same Tenant and dependencies already present in that
  Tenant. Catalog mutation and audit append share one transaction.
- Added `hako catalog list` and `hako catalog set`. Input is bounded strict
  JSON; it supports only the `lambda-microvm` runtime, validated language and
  service identifiers, bounded sorted dependencies, and optional HTTPS
  production URL metadata. No credentials or Secret references are accepted.
- Added DSQL migration 000138 and explicit `hako_api` table grants. Local
  unit/client/CLI and PostgreSQL integration tests cover RBAC, cross-Tenant
  hiding, Repository ownership, dependency validation, EC2 rejection, and
  unknown secret fields. See [`Software Catalog`](../docs/software-catalog.md).
- Added migration 000139 and Tenant RBAC APIs/CLI for a dormant OIDC policy.
  The allowlist requires an exact AWS STS audience, refs, environments and
  workflow refs. `POST .../deploy-policy/verify` validates the GitHub issuer's
  RS256 signature and exact Repository/Owner ID, ref/environment/workflow/event
  claims, including legacy and immutable-ID `sub` formats. Tokens are not
  stored; no AWS credentials are exchanged and every response keeps
  `execution_enabled=false`. Verification now fails closed unless a
  Tenant/Installation/Repository-scoped GitHub Environment approval verifier
  confirms an exact Environment `required_reviewers` rule. A reusable GitHub
  App client verifies a non-empty User/Team reviewer set with a short-lived
  `actions:read` token. The production API calls the Control Plane-only
  mTLS GitHub Credential Broker, which resolves Tenant App credentials and
  rechecks the policy pin after GitHub I/O. The broker receives read-only
  Software Catalog DSQL access. Unit/API integration coverage checks configured
  approval verification and missing-verifier failure; broker service and
  transport tests cover caller, pin, and denial behavior. Configured protection
  is not proof that a specific run was approved.
- Preview allocation/routing and PR lifecycle cleanup remain blocked by the
  listed runtime/deployment prerequisites. Production approval validation,
  IAM trust/exchange and deployment execution remain disabled.

## Preview lifecycle metadata slice (2026-10-24)

- Added default-off Tenant Repository Preview opt-in and Owner/Admin API at
  `PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/preview`.
- The durable inbox processor now applies PR opened/reopened/synchronize/closed
  events in the same transaction as delivery acknowledgement. State is keyed
  by Tenant, Installation, Repository, and PR; `updated_at` fences delayed old
  GitHub events, while duplicate Delivery IDs remain idempotent.
- Open PRs become `awaiting_runtime`; closed PRs become `cleanup_pending`.
  Migration 000140 adds the opt-in, and 000141 adds the lifecycle table.
  Runtime allocation is explicitly reported disabled; this change does not
  create a MicroVM, Workspace, or Gateway route.
- Migrations 000142–000146 record the fixed quota-based `expires_at` (24-hour
  fallback), and bounded `previews.ExpireDue` moves expired present records to
  `cleanup_pending` without claiming physical deletion. Reopen starts a new
  lifetime; synchronize does not extend it. PostgreSQL E2E verifies expiry and
  duplicate sweep idempotency. A default-off ARM64 Lambda and EventBridge
  schedule invoke the transactional bounded sweep; it has only a dedicated
  DSQL role and can update Preview cleanup metadata. Physical runtime cleanup
  worker remains unimplemented; no infrastructure was applied.
- Added a direct Preview store PostgreSQL test for cleanup claim exclusion,
  fixed backoff, expired-lease recovery, stale claimant rejection, confirmed
  completion, and duplicate acknowledgement. `make test-preview-expiry-integration`
  now runs it with the webhook lifecycle E2E; integration statement coverage
  for `internal/store/previews` is 37.4% (unit-only coverage is 21.2%). This
  remains metadata-store coverage, not a provider cleanup test.
- Added `internal/previewcleanup.Worker`, which claims bounded cleanup batches,
  invokes an injected idempotent executor with a deadline capped by the lease,
  records only fixed provider failure categories with retry/backoff, and marks a
  Preview cleaned only after executor confirmation. Its PostgreSQL integration
  test verifies that a failed MicroVM termination keeps the quota slot reserved,
  the retry uses a new claim/attempt, a later confirmed cleanup releases the
  slot, and a repeated sweep is idempotent. This is worker orchestration with a
  fake executor; Preview-to-runtime binding, AWS MicroVM/Gateway deletion, and
  a scheduled production composition are still unimplemented.
- Added PostgreSQL integration coverage for bounded Tenant/Installation/
  Repository-scoped Preview listing, list limits and ordering, foreign-scope
  isolation, expiry batch transitions, and repeated-sweep idempotency. Before
  quota admission additions, the focused suite reported 59.6% statement
  coverage for `internal/store/previews`; the current suite reports 45.4% after
  adding more store behavior. Runtime allocation and physical cleanup remain
  outside this coverage.
- `go test ./...` passed. PostgreSQL Preview lifecycle E2E passed against the
  local `hako-dev-postgres`, including stale delivery, close/reopen, Tenant
  scoping, and duplicate replay.
- Remaining: Preview Workspace/Run binding to Lambda MicroVM, expiring
  Tenant-scoped Gateway route, timeout/merge sweep, provider-backed MicroVM /
  route cleanup executor, and its scheduled composition. See
  [`Preview lifecycle`](../docs/preview-lifecycle.md).

## Preview lifecycle read API/CLI (2026-10-24)

- Added a bounded Tenant-member read endpoint and `hako github repository
  previews list` command for PR Preview lifecycle metadata. The response is
  scoped to the active registered Tenant Repository and exposes the immutable
  PR head, expiry, cleanup retry state, and timestamps; it does not expose
  provider identifiers and reports `runtime_enabled: false`.
- Added client/parser tests and PostgreSQL API integration coverage for member
  access, cross-Tenant hiding, and limit bounds. This improves visibility while
  Preview runtime allocation and physical cleanup remain unavailable.
- See [`Preview lifecycle`](../docs/preview-lifecycle.md) for endpoint and CLI
  usage.

## Preview cleanup retry state (2026-10-24)

- Added store transitions that record only fixed cleanup error categories and
  schedule exponential retries from 5 seconds up to 1 hour. Failures arriving
  before the stored retry deadline are ignored; attempts are guarded against
  stale concurrent writes.
- `MarkCleaned` is idempotent and changes metadata only after its caller has
  confirmed MicroVM and Gateway route deletion. Migrations 000145–000146 add a
  bounded cleanup claim lease; claim ID, attempt, and exact expiry fence completion and
  retry, while expired leases can be reclaimed after worker restart. Unit and
  PostgreSQL lifecycle tests cover backoff, premature retry rejection, stale
  claim rejection, lease recovery, repeated failure, and duplicate completion.
- Migration 000147 adds a monotonic Preview lifecycle generation. Accepted PR
  head/desired-state changes and expiry advance it; cleanup retry and completion
  require the generation captured by the claim. The bounded list API exposes
  the generation, and PostgreSQL integration coverage checks stale generation
  fencing across PR update, close/reopen, expiry, retry, and cleanup.
- The generic cleanup worker now executes the claim/retry/confirmation state
  machine, but Preview-to-runtime binding and provider/Gateway deletion remain
  unimplemented. No delete is inferred from the metadata transition.

## Preview concurrency admission (2026-10-24)

- PR Preview open/reopen now advances the shared Tenant quota OCC guard and
  checks capacity in the webhook transaction. The same configured limit is
  shared with Workload Preview Runs. A full quota persists a
  `desired_state=absent,state=quota_exceeded` record and does not create a
  cleanup job or runtime allocation. A later `reopened` event retries admission.
- Active and cleanup-pending PR Preview records count against usage and block
  quota reductions until cleanup is confirmed. A rejected PR consumes no slot;
  closing it records `cleaned` directly. The Usage API reads the combined
  reservation count. Migration 000156 extends the lifecycle-state constraint.
- PostgreSQL integration covers an active cleanup reservation rejecting a
  second PR and a Workload Preview, followed by successful admission after the
  first Preview is cleanup-confirmed. These checks cover quota metadata;
  runtime allocation, Gateway routing, and provider-backed deletion remain
  unimplemented.

## Acceptance criteria

- Tests cover PR update/reopen/close races, stale commit preview, cross-Tenant route access, expiry, cleanup retries, and duplicate webhooks.
- Catalog reads/writes enforce Tenant RBAC and validate dependency references.
- OIDC token validation rejects wrong issuer/audience/repository/ref/environment/workflow; enabled policies require configured Environment reviewer verification and fail closed when that verifier is unavailable. No cloud credentials are stored or STS exchange enabled.
- Document cost/quota, preview data isolation, cleanup, and production approval policy.
