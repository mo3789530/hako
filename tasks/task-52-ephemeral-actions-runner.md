# Task 52: GitHub Actions Ephemeral Runner Mode

Status: In Progress (2026-10-24: signed `workflow_job.queued` deliveries are retained but excluded from the generic push/PR Go-test trigger. Normalized events preserve run attempt, workflow name, bounded labels, and optional assigned runner/group identity. Tenant Owner/Admin repository opt-in is stored via API/CLI and audited. A separate default-off admission flag revalidates Tenant/Installation/Repository policy and persists deduplicated queued job admissions; worker claims use expiring leases with recovery. A Control Plane-only mTLS broker endpoint consumes a claim, while a Resource Plane-only bootstrap-token mTLS endpoint requires the exact live Plane+MicroVM binding and a transactionally consumed one-token issuance timestamp. GitHub-observed runner assignment and completion are persisted against the exact admitted job. The starting claim pins the Lambda MicroVM and Resource Plane identities; terminal cleanup outbox records preserve both, cleanup leases are Resource Plane-scoped, and a locally tested cleanup worker/transactional queue adapter handles termination→deregistration→fenced acknowledgement with bounded retry. An AWS SDK Lambda MicroVM terminator is implemented with exact Plane fencing, identity verification, idempotent state handling, and bounded polling. A separate Plane-allowlisted mTLS broker endpoint resolves deregistration coordinates only from an unexpired cleanup claim. The Lambda MicroVM launch adapter validates trusted image/network/role profiles, uses a deterministic claim-scoped idempotency token, and sends only non-secret job metadata in the Run Hook. A Resource Plane-allowlisted mTLS binding endpoint persists the launched VM ID against the exact live starting claim, and startup-failure cleanup preserves both Plane and VM identity. A local signed bootstrap protocol delivers the short-lived runner token only to the exact HTTPS MicroVM endpoint using an AWS auth token limited to port 8080 and at most five minutes; a guest-side verifier rejects tampering, wrong-VM binding, and expiry. A default-off SQS publisher claims durable admissions and publishes only job/attempt/claim identity; the Control Plane Outbox Dispatcher can run it on its existing schedule. The Resource Plane Lambda composition and partial-batch consumer are locally fake-tested. Durable queued/in-progress/completed Check state is correlated to the exact integration/job/attempt and delivered through the Checks Outbox/queue/consumer; a Control Plane-only mTLS endpoint issues repository-scoped Checks credentials, and startup failure produces a terminal failure Check. The composed PostgreSQL lifecycle covers signed queued→in-progress→completed webhook, admission/claim, fenced startup, signed Guest bootstrap, fake runner success/failure/cancel/timeout, durable Check publication, and termination/deregistration retry after worker restart. `make test-actions-runner-lifecycle-integration` passed on 2026-10-24. The ARM64 guest image now builds from the official digest-pinned Runner 2.338.1 base; it is inspected locally but not executed. Resource Plane Terraform remains default-off; deployed listener/network, AWS apply/review, multi-Plane placement, and live AWS/GitHub verification remain. Priority: P3.

## GitHub Checks annotation transport coverage (2026-10-25)

The GitHub Checks API client integration test now sends a bounded,
repository-relative annotation alongside Check output and verifies the exact
path and line survive JSON serialization to the Checks endpoint. Existing
validation rejects traversal paths and excessive annotation counts. This
low-level transport is now wired through signed Workload pipeline results,
bounded result validation, JSONB persistence, and the durable Workload Check
consumer. The PostgreSQL result-handler test verifies annotation persistence,
and the PostgreSQL Outbox/Consumer test verifies delivery to the fake GitHub
Checks API. Actions Runner Check producers still publish summary-only results.

## Runner diagnostics progress (2026-10-25)

The guest now collects bounded regular `.log` files from the official runner's
`_diag` directory before deleting ephemeral runner state. It skips symlinks,
caps collection at 64 KiB, redacts the exact registration/model credentials
and common GitHub/API token forms, computes a SHA-256 digest, and exposes the
redacted bytes through a one-shot process result callback. The Resource Plane
startup claim carries a stable capability derived from the high-entropy claim
ID; only its SHA-256 digest is persisted. The signed guest bootstrap includes
the exact HTTPS upload endpoint and capability. After collection, the Guest
retries a bounded upload before deleting `_diag`; Control Plane API validates
the request digest, applies its own redaction pass, and writes only the redacted
3 KiB excerpt and full-log digest to the immutable job/attempt Check row. The
Check Outbox then updates the exact commit Check. Identical upload replay is
idempotent; a changed payload is rejected. Unit and PostgreSQL integration
tests verify the capability, commit binding, redaction, retries, and outbox.
The full logs are not retained in DSQL/S3. The endpoint must be reachable via
the configured MicroVM egress connector; deployed network/Lambda verification
remains unperformed.

Claude Code Action support is tracked separately in [Task 73](task-73-claude-code-action.md), including Tenant-scoped model credentials, untrusted PR denial, and action-specific E2E.

## Workflow policy read API (2026-10-25)

Added a Tenant-member GET endpoint and `hako github repository runner policy
show` command for reading an enabled repository's configured workflow
allowlist. The response contains policy data only. Cross-Tenant and disabled or
unconfigured repositories return not found; configuration remains Owner/Admin
only. The API/CLI contract tests and PostgreSQL integration test verify member
read, foreign-Tenant hiding, policy persistence, and policy removal visibility
after runner opt-out. This does not enable runner execution or alter its
default-off state.

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
- `RunnerStartupHandler` exposes the ordered worker as an AWS Lambda SQS event
  handler. It accepts only the claim key fields, rejects oversized/unknown
  message properties, and returns per-record batch failures so a malformed or
  transiently failed record does not replay successful records in the same
  batch. Tests verify the worker sequence and poison-record isolation. A
  production SQS event-source mapping and command publisher/dispatcher remain
  unimplemented and default-off; Lambda runtime composition is tracked below.
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
  PostgreSQL tests. This is a library-level startup worker: the guest
  listener/image is not wired, and no live AWS call has been made.

- `RunnerGuestRuntime` now handles the Lambda MicroVM `/run` lifecycle wrapper,
  validates its strict non-secret launch payload, and binds the exact provider
  `microvmId` plus job/run/attempt/repository/runner-name/labels before
  enabling the bootstrap route. Identical lifecycle retries are idempotent;
  foreign job envelopes and conflicting rebinds fail closed. This remains a
  unit-tested library handler, now composed by the guest executable below.

- `cmd/hako-actions-runner-guest` composes the lifecycle/runtime handler and a
  fake-tested process consumer. It configures the official Actions runner with
  `--ephemeral`, passes `ANTHROPIC_API_KEY` only to the runner process (not its
  registration/configuration command), supplies no inherited AWS environment,
  and removes runner credential files, diagnostics, temp files and checkout
  state after exit. The ARM64 image builds from the official Runner 2.338.1
  image pinned by platform digest and adds only the Hako guest executable and a
  public bootstrap key. The local x86_64 host can build and inspect the image
  but cannot execute it without ARM emulation. No real GitHub/AWS calls were
  made.

- `cmd/hako-actions-runner-lambda` now provides the Resource Plane Lambda
  runtime entrypoint for the SQS startup handler. It requires an HTTPS broker,
  fixed MicroVM image/version, restricted egress connector, Resource Plane
  identity, and four distinct Secrets Manager ARN references for the broker
  client certificate/key, trust bundle, and Ed25519 bootstrap signing key.
  Startup loads those values through the Lambda execution role, bounds their
  size, validates mTLS and signing-key material, and composes the broker
  clients, Lambda MicroVM SDK launcher/auth-token provider, signed bootstrapper,
  startup-failure reporter, and partial-batch SQS handler. Tests use fake
  Secrets Manager and Lambda APIs; no AWS endpoint or real Secret was accessed.
  `make build-actions-runner-lambda` packages the ARM64 custom-runtime ZIP.
  The Terraform deployment, queue/DLQ, cross-account policy, exact worker IAM,
  and event-source mapping are default-off and have not been applied. The guest
  listener is included in the digest-pinned ARM64 image, and image build/inspect
  succeeds locally; execution in an ARM64 environment or Lambda MicroVM, plus
  live AWS/GitHub verification, remains unverified.
  Durable GitHub Check delivery is locally implemented
  and PostgreSQL/fake-GitHub tested; Runner admission stays default-off. The dispatch
  queue contract and its single-Plane limitation are recorded in
  [Actions Runner startup queue](../docs/actions-runner-startup-queue.md).
- Local verification refresh (2026-10-18): `GOCACHE=/tmp/hako-go-cache make
  test-actions-runner-lifecycle-integration` passed for the full signed webhook,
  PostgreSQL admission, Guest fake-runner, Checks, and cleanup-recovery lifecycle.
  It uses synthetic credentials, fake providers, and isolated DB schemas; no
  real GitHub, AWS, or Secrets Manager endpoint is contacted.
- `RunnerBootstrapReceiver` adds the guest-side receiver primitive: it accepts
  only bounded strict JSON, verifies the signed envelope against the exact
  MicroVM ID and expiry, marks a single delivery consumed before invoking a
  synchronous consumer, and clears the model-key byte buffer afterward. Tests
  cover wrong-VM signed envelopes, malformed bodies, duplicate requests,
  consumer failure and buffer erasure. The receiver is included in the pinned
  runner image; the broker→Worker→guest→process path and cleanup are composed
  in Task 77's fake-provider PostgreSQL lifecycle. The ARM64 image itself has
  not been executed on this x86_64 host or in a Lambda MicroVM.
