# Task 52: GitHub Actions Ephemeral Runner Mode

Status: In Progress (2026-10-17: signed `workflow_job.queued` deliveries are retained but excluded from the generic push/PR Go-test trigger. Normalized events preserve run attempt, workflow name, bounded labels, and optional assigned runner/group identity. Tenant Owner/Admin repository opt-in is stored via API/CLI and audited. A separate default-off admission flag revalidates Tenant/Installation/Repository policy and persists deduplicated queued job admissions; worker claims use expiring leases with recovery. A Control Plane-only mTLS broker endpoint consumes a claim, while a new Resource Plane-only bootstrap-token mTLS endpoint requires the exact live Plane+MicroVM binding and a transactionally consumed one-token issuance timestamp. GitHub-observed runner assignment and completion are persisted against the exact admitted job. The starting claim pins the Lambda MicroVM and Resource Plane identities; terminal cleanup outbox records preserve both, cleanup leases are Resource Plane-scoped, and a locally tested cleanup worker/transactional queue adapter handles termination→deregistration→fenced acknowledgement with bounded retry. An AWS SDK Lambda MicroVM terminator is implemented with exact Plane fencing, identity verification, idempotent state handling, and bounded polling. A separate Plane-allowlisted mTLS broker endpoint resolves deregistration coordinates only from an unexpired cleanup claim. The Lambda MicroVM launch adapter validates trusted image/network/role profiles, uses a deterministic claim-scoped idempotency token, and sends only non-secret job metadata in the Run Hook. A Resource Plane-allowlisted mTLS binding endpoint persists the launched VM ID against the exact live starting claim, and startup-failure cleanup preserves both Plane and VM identity. A local signed bootstrap protocol delivers the short-lived runner token only to the exact HTTPS MicroVM endpoint using an AWS auth token limited to port 8080 and at most five minutes; a guest-side verifier rejects tampering, wrong-VM binding, and expiry. Production guest listener/image wiring, claim-aware worker composition/deployment, GitHub Check reporting, and live AWS/GitHub verification remain. Priority: P3.

Claude Code Action support is tracked separately in [Task 73](task-73-claude-code-action.md), including Tenant-scoped model credentials, untrusted PR denial, and action-specific E2E.

## Goal

Offer Hako as an ephemeral self-hosted runner backend for existing GitHub Actions workflows without taking ownership of or rewriting user workflow YAML.

## Work

- Consume only authorized `workflow_job.queued` events and correlate repository, workflow, ref, commit, labels, Tenant policy, and Check state.
- Keep the per-repository `actions_runner_enabled` policy separate from Factory event triggers; an opt-in must never itself issue a token or create runtime resources.
- Register one ephemeral runner for one job, wait for the exact assigned job, capture diagnostics, deregister it, and destroy the isolated execution environment on every terminal path.
- Apply trust policy: fork PR has no secrets/cloud role; privileged labels require explicit policy/approval; no AWS credentials are injected as long-lived keys.
- Implement bounded runner registration token lifetime, capacity, retry, queue timeout, and GitHub API throttling.

## Acceptance criteria

- Fake GitHub tests cover duplicate queued events, runner assignment mismatch, startup failure, cancellation, timeout, job completion, deregistration failure, and cleanup retry.
- Tests prove one runner accepts at most one job and never processes a second repository/job.
- Runner logs are collected/redacted before ephemeral disk deletion and attached to the correct commit Check.
- Permissions are separately reviewed from ordinary Repository checkout and scoped to the minimum Actions/Administration APIs required.

## Progress

- The shared webhook inbox continues to normalize and retain `workflow_job`
  deliveries for the dedicated runner path, but generic Repository
  verification Runs now accept only push/pull-request events. A queued Actions
  job therefore cannot accidentally start Hako's default `go test ./...`
  workload. The future runner admission path must independently resolve
  Tenant/Repository policy; no runner token, runtime, or Check is created by
  the generic trigger.
- `WorkflowJobRef` now retains the workflow/job/run IDs, positive `run_attempt`
  when provided, workflow name, exact bounded runner label strings (maximum
  100), and optional runner/group assignment IDs and names. Action/status
  mismatches, invalid label controls/lengths, and non-positive supplied
  IDs/attempts fail normalization.
  Queued jobs with null runner assignment remain unassigned; later events can
  be correlated against the exact job and run attempt. Older normalized rows
  remain readable because these fields are additive/optional.
- Runner admission now requires a positive `run_attempt`, even though event
  normalization retains older payloads where GitHub omitted the field. Such
  queued deliveries are quarantined without creating a job/runtime; status
  events missing an attempt are likewise prevented from updating a job. This
  avoids persisting a zero-attempt job that cannot correlate with normal
  `in_progress`/`completed` deliveries. PostgreSQL integration coverage verifies
  both paths and confirms a terminal job cannot be altered by an uncorrelatable
  status event.
- A GitHub Runner ID is now single-job within its exact Tenant App installation
  and repository scope. An idempotent database uniqueness constraint closes
  concurrent races, while status processing quarantines an assignment to a
  second job before updating it. The PostgreSQL E2E covers attempted reuse
  after the original job has completed.
- A terminal `workflow_job.completed` transition now inserts an idempotent
  cleanup request in the same PostgreSQL transaction. Cleanup jobs have
  bounded claim leases, retry scheduling with operator-safe error categories,
  and stale-worker fencing; tests cover claim exclusion, retry/reclaim,
  completion, and one outbox row for the terminal job. This is durable queue
  plumbing only: the Lambda MicroVM cleanup/deregister worker still needs
  implementation.
- The starting Runner claim can bind a single immutable Lambda MicroVM provider
  ID and Resource Plane ID under its live claim lease. Repeating the same
  binding is idempotent; replacing either identity or binding from a
  stale/terminal claim is rejected. Terminal status processing copies both
  identities into the cleanup outbox transaction. Cleanup claims are scoped
  to the worker's exact Resource Plane; unbound jobs with no VM may be claimed
  for repository-scoped runner deregistration only, while a VM ID without a
  Resource Plane pin fails closed.
  This is only the durable identity handoff: no Lambda MicroVM launch,
  GitHub deregistration, or physical cleanup API is invoked yet.
- If registration-token issuance or startup fails after the claim enters
  `starting`, the broker's failure transition now inserts the cleanup outbox
  row in the same PostgreSQL transaction and preserves any already-bound
  Resource Plane/Lambda MicroVM pair. If no MicroVM was allocated, the cleanup
  lease can still be used for repository-scoped deregistration; a MicroVM ID
  without its Resource Plane is never made claimable. This closes the failure
  window between VM allocation and runner readiness; a cleanup consumer is
  now represented by `internal/actionsrunner.CleanupWorker` and its
  PostgreSQL-backed lease queue. `SDKLambdaMicroVMTerminator` terminates the
  exact pinned VM before tenant/repository-scoped deregistration; duplicate
  external effects are expected after a crash and therefore adapters must be
  idempotent. Deregistration uses a Resource Plane-specific mTLS client; the
  broker revalidates the exact lease/Plane and resolves Tenant/Repository data
  from PostgreSQL. Real AWS/GitHub requests have not been issued, and the
  worker deployment remains unimplemented.
- The Tenant App token client now supports exact repository runner removal via
  GitHub's repository self-hosted-runner API using only a short-lived,
  repository-scoped `administration:write` Installation token. Cleanup remains
  authorized by the active Tenant App/repository binding even after the Runner
  admission opt-in is turned off; the runner ID must come from the durable
  cleanup job. An already-absent runner is treated as idempotent completion
  after the exact Installation token and repository authorization are checked.
  The real consumer and its transport are not yet connected.
- `HAKO_GITHUB_ACTIONS_RUNNER_ADMISSIONS_ENABLED` defaults off. When enabled,
  the webhook processor admits only queued jobs whose current Tenant-owned
  Installation and Repository opt-in were active before receipt. Admission is
  idempotent in `github_actions_runner_jobs`; a single controller can claim a
  row with a bounded lease and another controller can recover it after expiry.
  Admission/claim do not issue credentials or launch compute. A dedicated
  controller is required to execute and transition claimed rows.
- The trusted GitHub token client has a fake-tested registration-token method
  that rechecks the exact Tenant App revision/generation and Runner opt-in,
  requests only repository-scoped `administration:write`, and avoids caching
  both the broader Installation token and the one-hour runner token. The
  Control Plane-only mTLS route accepts a claim key (not Tenant/Repository
  coordinates), atomically consumes `claimed` into `starting`, and only then
  resolves the pinned Tenant App and repository for token issuance.
- Authorized `workflow_job.in_progress` and `completed` deliveries are now
  processed by the separate default-off admission path. They update a durable
  job only when Tenant, Installation, Repository, workflow run/job/attempt,
  branch, and commit SHA match the queued admission. The assigned Runner ID is
  pinned on first `in_progress`; assignment mismatches and out-of-order status
  events cannot replace or reopen a job. GitHub conclusion and completion time
  are retained, and terminal transitions clear claim leases. Migration `000081`
  adds observed runner/result fields. This records GitHub-observed state only;
  it is not yet connected to the Lambda MicroVM launcher, a runner Check, or
  physical cleanup retry.

- `internal/actionsrunner.LambdaRunnerLauncher` now invokes the official
  Lambda MicroVM SDK using a trusted, fixed image/version, restricted egress
  connector, optional same-account execution role, and an eight-hour-bounded
  runtime. The stable client token is derived from Integration/job/attempt/
  live claim, so retries address the same provider allocation. The `/run`
  payload contains only repository URL, bounded job identity, runner name, and
  labels; the registration token is intentionally absent. Provider output is
  checked for exact image, version, duration, role, egress connector, state,
  and MicroVM-specific HTTPS endpoint. Unit tests cover idempotent retry and
  reject expired claims, unsafe labels, mismatched provider configuration,
  invalid endpoints, and provider failures. This is a launch adapter contract
  only. The broker-side binding is now available over a Plane-allowlisted
  mTLS route. The new `RunnerStartupWorker` composes this binding with the
  launch and bootstrap-token clients; no AWS call is made by tests.
- The Resource Plane can persist a launched VM identity without a direct
  Control Plane database connection. `POST /v1/actions-runner/bind` authenticates
  a configured Resource Plane SPIFFE identity, derives Plane ID from the
  certificate (not JSON), and transactionally calls the existing live-claim
  binder. The mTLS client sends only Integration/job/attempt/claim and provider
  VM IDs. Cross-Plane/stale-claim/replacement attempts fail closed. The broker
  startup-failure transition now copies both `resource_plane_id` and
  `lambda_microvm_id` into its cleanup outbox, verified by PostgreSQL
  integration coverage. `/v1/actions-runner/bootstrap-token` is now a separate
  Resource Plane mTLS route: it requires the caller's exact Plane to match the
  live starting claim's durable Plane+MicroVM binding, and a new DSQL field
  atomically permits only one token-mint attempt per workflow attempt. The
  older registration endpoint remains Control Plane-only. A Resource Plane
  mTLS client returns the short-lived opaque grant for the signed bootstrap
  sender; the actual queue consumer and guest listener remain unconnected.
- `POST /v1/actions-runner/launch` now consumes a live claimed job into
  `starting`, assigns its Resource Plane from the verified SPIFFE identity,
  and returns repository/job metadata without secrets. A lost response can be
  replayed by the same Plane while the claim is live and before token issuance;
  another Plane or later-stage replay fails closed. The new
  `RunnerStartupWorker` enforces launch → bind → token broker → signed guest
  bootstrap, and reports any post-claim failure through the Plane-only
  `POST /v1/actions-runner/startup-failed` route. That route atomically marks
  only the exact live starting claim failed and writes its cleanup outbox row;
  retries are idempotent only when that Plane's failure cleanup already exists.
- `RunnerBootstrapper` now requests a Lambda MicroVM auth token scoped to the
  fixed runner-bootstrap port 8080 and at most five minutes (one minute when
  the claim lease is short), then posts a signed,
  job/attempt/VM-bound envelope to the exact provider HTTPS endpoint. The
  envelope is capped by the token expiry and five minutes, and the HTTP client
  refuses redirects; AWS proxy auth is sent only as a header and the GitHub
  registration token is absent from Run Hook metadata. `VerifyRunnerBootstrap`
  provides the guest-side Ed25519/VM/expiry checks. Fake SDK and transport tests
  cover exact port scope, signature/tamper rejection, wrong VM, expiry, unsafe
  endpoint rejection before auth-token issuance, and the successful HTTPS
  request contract. Worker ordering and cleanup reporting have fake-backed
  local tests, and launch/token/failure endpoints are covered by mTLS and
  PostgreSQL tests. This remains library-level composition: no SQS consumer,
  guest listener/image wiring, or AWS call is implemented.
