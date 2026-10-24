# Task 53: Native Pipeline and Issue-to-Agent Workflow

Status: In Progress (versioned manifest selection, fixed-catalog execution, durable per-pipeline results, and independent pipeline Checks through the existing Checks consumer are implemented and locally verified. Issue command authorization requires a verified GitHub identity that is a member of the resolved Tenant, an active Installation binding, exact repository identity, and a pre-existing Factory opt-in. A Tenant-App-only read scope, bounded Issue/default-branch SHA reader, Control Plane-only mTLS broker, and an atomic Issue inbox processor that creates one immutable Agent Run plus a bounded task record are locally composed and PostgreSQL-tested. The Resource Plane Worker now retrieves the bounded task over mTLS and validates current attempt and immutable Run/App/Repository/branch/commit pins. A context digest is bound from launch specification through signed Start/Source tickets and guest `/run`; guest checks it before exposing task text in memory to a process factory. Scheduling remains disabled (dispatcher/batch are Job-only), and the default guest process factory rejects Agent Runs until an isolated model-backed executor, Tenant credential policy, and runtime resource/network enforcement exist. Service container orchestration and live Agent write execution remain; branch/PR policy is implemented as dormant configuration). Priority: P3.

Runtime profile follow-up: profiles are now keyed by Workload kind, Runtime Class,
and trust level. A Job profile cannot be implicitly reused by Agent or Preview;
missing kind-specific profiles fail before source materialization. Existing
profile JSON without `kind` maps to `job` for compatibility. This is an
additional fail-closed boundary, not a model executor or runtime network
isolation proof.

## Goal

Allow Hako-native verification and bounded coding-agent Jobs without silently replacing GitHub Actions or granting agents production authority.

## Work

- Define versioned `.hako/factory.yaml` with strict schema, allowlisted steps/images/services, bounded resources/time, explicit network policy, and secret scopes.
- Reject unknown fields, shell privilege escalation, unsafe mounts, mutable/unapproved images, and policy bypasses before scheduling.
- Add Issue/comment command to create an Agent Run in a separate ephemeral runtime; default permissions are repository read and no cloud secrets.
- Require an explicit policy for branch push/PR creation; use short-lived GitHub App installation token with narrow Contents/Pull Requests permissions and only after tests pass.
- Keep agent output labeled as untrusted proposal; human review remains required for merge/deploy.

## Acceptance criteria

- Parser/policy tests cover malicious YAML, aliases/anchors limits, shell injection, fork/untrusted network, secret exfiltration, oversized pipeline, and unsupported version.
- End-to-end fake tests prove Issue replay does not create duplicate Agent Runs/PRs and agent cannot deploy or merge.
- Every write action is scoped to the Tenant-bound Repository, branch policy, and short-lived token; tokens are absent from process output/artifacts.
- Document coexistence with GitHub Actions and migration/disable behavior.

## Local progress (2026-10-17)

- Added a transactional Issue-command authorization resolver. It accepts only a
  normalized `issue_comment.created` `/hako implement` command on a regular
  Issue from OWNER/MEMBER/COLLABORATOR, revalidates the active Installation and
  tenant integration, resolves the Tenant's exact Repository, requires the
  repository Factory opt-in to predate delivery receipt, and maps the verified
  GitHub numeric sender ID to a current `tenant_members` row. It returns only
  Hako Tenant/User and GitHub Issue identity; it neither consumes the delivery
  nor creates a Run. PostgreSQL tests cover an authorized member, a GitHub user
  who belongs only to another Tenant, and a PR comment. The dedicated processor
  remains disabled until it can resolve issue context and an immutable SHA and
  persist a deduplicated Agent Run atomically.
- Added a Tenant-App-only token scope for Issue context lookup. It requests
  exactly `contents:read` and `issues:read` for one repository, includes the
  Tenant integration/revision/generation in its distinct cache key, and
  revalidates the active App and repository before and after minting. A bounded
  GitHub API reader fetches only an open non-PR Issue and its current default
  branch tip, returning the immutable commit SHA; redirects, oversized payloads,
  malformed metadata, and invalid SHA values fail closed. Fake GitHub tests
  verify scopes, Tenant fencing, commit resolution, and PR/SHA rejection. This
  library is connected to the Control Plane-only `/v1/agent/issue-context`
  mTLS endpoint and HTTP client. The endpoint accepts only Integration and
  signed Delivery IDs, re-resolves DB authorization before and after GitHub
  requests, and returns no-store bounded Issue context. PostgreSQL integration
  coverage verifies credential pin resolution and membership revocation.
- Added a separately callable Issue-command batch processor. It preflights
  Tenant membership and repository opt-in before the broker call, then repeats
  authorization in the final transaction. That transaction creates/reuses an
  Agent Run with the immutable SHA and delivery-scoped idempotency key, stores
  bounded Issue context in `workload_agent_tasks` (not the webhook inbox or
  user-facing Run response), and marks the delivery processed atomically.
  PostgreSQL integration verifies replay creates one Run/task. The batch is not
  attached to Lambda scheduling until the MicroVM Agent executor supports
  `kind=agent`.

- Added `internal/factoryconfig` version 1 parser with strict `push` and
  `pull_request` triggers, bounded branch filters, and a deterministic selector
  over normalized webhook events. The selector is not connected to scheduling.
- Added `internal/factoryconfig` version 1 parser with unknown-field rejection,
  duplicate-key detection, alias/anchor/tag rejection, single-document and
  structure-size limits, fixed catalog step/service allowlists, bounded runtime
  resources, repository-read-only access, and no cloud credential access.
- Added `hako factory validate [--file path]`, which validates the default
  `.hako/factory.yaml` or an explicit file locally; output contains only the
  version and sorted pipeline names.
- Added a deterministic `factoryconfig.Plan` which maps validated catalog
  steps to fixed executable/argument vectors, omits checkout as a process,
  and carries bounded runtime/network/service metadata without aliasing parser
  memory. It does not evaluate shell text.
- Persisted the normalized webhook trigger event and selection branch on the
  immutable Workload Run (push branch or pull-request base branch), included
  them in the idempotency fingerprint, and exposed them in authenticated
  Resource Plane execution metadata schema v2 for later fixed-commit manifest
  selection. The client retains compatibility with v1 metadata for older Runs.
- Signed the trigger context into Agent ticket schema v2 and Lambda MicroVM
  run-hook identity; legacy tickets remain schema v1. The Workload Agent
  verifies the source digest/install identity before reading `.hako/factory.yaml`,
  selects matching pipelines deterministically, and runs only fixed catalog
  executable/argument vectors in sorted pipeline order. Missing manifest uses
  the legacy Go-test path; malformed or unmatched manifests fail closed.
- Invalid manifests become a sanitized terminal Workload failure instead of a
  retryable Agent error. Focused tests cover fixed-commit selection, ticket
  compatibility, symlink rejection, deterministic catalog execution, and this
  terminal failure path.
- Native pipeline execution now records a bounded result per selected pipeline
  (name, terminal status, exit code, and safe error code). The signed Agent
  result validator enforces sorted unique allowlisted names and status/code
  consistency; PostgreSQL stores these rows in the same transaction as the
  terminal Run transition. Authenticated Run detail returns only the latest
  attempt generation's results. Local Agent, store, and PostgreSQL integration
  tests cover this path.
- Pipeline result application now also creates one durable completed GitHub
  Check per pipeline (`hako/<kind>/<pipeline>`), keyed separately from the
  aggregate Run Check. Each Check has a pipeline-scoped Outbox envelope and
  is loaded, published, and assigned its own Check Run ID by the existing
  retryable Checks consumer. A schema migration preserves the legacy Run-level
  Check as the empty pipeline key. PostgreSQL verifies the composite key,
  result-to-Check transaction, conclusions, and Outbox; consumer tests verify
  named delivery. These Checks are published when the Run completes, not as
  streaming per-pipeline progress.
- Added malicious YAML/resource-bound tests and documented the schema in
  [`docs/software-factory.md`](../docs/software-factory.md). The Agent currently rejects service containers, `network: none`,
  non-standard runtime classes, and does not translate CPU/memory bounds into
  MicroVM controls. Issue-to-Agent/PR workflow remains unimplemented.
- Added a process-level regression test proving an expired execution context
  terminates a native pipeline and records the bounded `timed_out` result
  (`pipeline_timeout`) without waiting for the manifest's minimum timeout.
  It exposed and fixed an exit-code mismatch where the pipeline result was
  timed out (`124`) but the enclosing process returned cancellation (`130`).
- Added a cancellation test that waits until the catalog process has started,
  cancels the Run, and verifies the active Pipeline is `cancelled` while later
  Pipelines are durably reported as `skipped`.
- Added a failing-step test that checks the original bounded exit code is
  retained, the Pipeline is reported as `failed`, and later Pipelines do not
  run after the failure.
- Deadline expiry and explicit cancellation now share one classifier at both
  command exit and final-step boundaries, so an already-finished step cannot
  turn a Run deadline into a cancelled Pipeline (or vice versa).
- Added a closed `/hako implement` comment-command parser. It accepts only
  exact bounded command text (ignoring surrounding whitespace) and rejects
  appended prompt text; the command does not itself authorize a user or create
  a Run. Issue-comment webhook scheduling, immutable commit resolution,
  collaborator policy, Agent runtime execution, and explicit branch/PR write
  policy remain unimplemented.
- Connected the closed command parser to verified `issue_comment.created`
  normalization. The stored projection contains only Issue/comment IDs, PR
  classification, GitHub author association, and the command enum; command
  recognition is limited to OWNER/MEMBER/COLLABORATOR associations. Inbox
  persistence redacts comment body while retaining the original signed-byte
  hash for Delivery ID idempotency. This still does not authorize a Hako Tenant
  member, schedule a Run, resolve a fixed commit, execute an Agent, or enable
  branch/PR writes.
- The generic repository-trigger processor deliberately does not claim
  `issue_comment` rows. A PostgreSQL regression test proves even an OWNER's
  exact command cannot enter the push/PR Job creator. Before enabling a
  dedicated processor, Hako must recheck current Tenant membership; GitHub
  `author_association` is repository metadata, not Hako authorization. The
  existing PKCE Installation setup now obtains the verified GitHub numeric user
  ID/login through the transient OAuth user token and links it atomically to the
  Cognito-backed Hako user. A numeric GitHub identity is unique across Hako
  users and cannot be silently replaced. `issue_comment` normalization also
  retains the provider's numeric sender ID for lookup. The broker-backed
  context resolver and atomic Agent Run processor now exist. They remain
  disconnected from the scheduled Lambda until the Resource Plane supports
  isolated Agent execution; delivery ID plus integration is the idempotency
  key.

- Added a separate broker `/v1/agent/task` read endpoint for the Worker
  path. It requires a verified, explicitly allowlisted Resource Plane mTLS
  identity and an exact Tenant/Run/attempt/generation/Plane tuple. The database
  resolver returns only an active latest Agent attempt and checks task
  commit/default-branch/App/repository pins against the Run. The broker has
  read-only access to task records; the API role does not. Its bounded response
  is `no-store`. The Worker now calls this endpoint over mTLS, validates the
  task against the exact active attempt, and fails closed before source upload
  when task retrieval or pin validation fails. Agent dispatch and model
  credential delivery remain disabled.
- Resource Plane execution metadata schema v3 now carries `kind` and the
  correlation ID. Legacy v1/v2 metadata remains Job-compatible; v3 accepts
  only Job or Agent, and Agent metadata requires an `issue_comment` trigger
  and a Tenant App credential pin. The Control Plane resolver, Resource Plane
  materializer, and launch-spec hash preserve the kind. Unit and PostgreSQL
  tests cover Job compatibility and Agent metadata resolution. The context
  digest is also signed through launch/runtime binding and checked against the
  Start payload in the guest. The shared canonical digest covers all Tenant,
  Run, App/Delivery, Repository, Issue/comment, branch/commit, title, and body
  fields; Control Plane persistence, Broker validation, Worker, and guest use
  the same implementation. Issue text is bounded and memory-only, excluded
  from journal/result/log data. Agent scheduling remains disabled; the default
  guest process factory returns a sanitized terminal
  `agent_executor_unavailable` failure for Agent Runs.

## Local verification refresh (2026-10-24)

- Added a shared `internal/agenttask` canonical digest implementation used by
  Control Plane persistence, Broker validation, Resource Plane Worker, and
  MicroVM Agent. It binds all immutable task identity and Issue fields rather
  than only title/body/branch/commit. A regression test mutates each field and
  verifies the digest changes.
- `go test ./...` passes. Focused PostgreSQL integration tests for Agent Issue
  admission, Broker task retrieval, Workload metadata, and attempt pinning pass
  against the already-running local PostgreSQL. `git diff --check` passes.
- Agent dispatch remains intentionally unavailable: the generic dispatcher
  and scheduler accept Jobs only, and the default guest factory rejects Agent
  work until the model-backed executor, Tenant credential policy, and runtime
  resource/network policy are implemented. No AWS apply or real Secret read
  was performed.
- Resource Plane profile selection now includes `WorkloadKind`. Tests prove an
  Agent Run cannot borrow the otherwise valid Job profile and is rejected
  before repository source materialization. Profile configuration defaults an
  omitted `kind` to `job`; explicit `agent` profiles are accepted only as a
  separate identity. This does not enable Agent scheduling or prove that
  profile connectors implement an egress restriction.
- Factory validation now totals the immutable Service Catalog memory/CPU
  requests against the declared pipeline budget and rejects sidecars that
  exhaust either budget or use `network: none`. This is admission planning
  only; the Workload Agent still has no Job-scoped Service provisioner or
  runtime enforcement for the pipeline process budget.
- Added a regression test that first proves a valid `postgres` sidecar plan
  passes manifest admission, then verifies the Workload Agent refuses to run it
  until a Job/attempt-scoped executor is composed. This protects the boundary
  against accidental reuse of Workspace-owned Service IDs or volumes; it does
  not complete service orchestration.

## Remaining runtime implementation gates

The current Workspace Service Engine is not a Job service runtime: it uses a
Workspace identity, durable Service IDs/volumes, and Workspace lifecycle
commands. The Workload Agent image also does not include the rootless
containerd toolchain. Do not enable manifest `services` by reusing that path.
The Job runtime needs its own implementation with these acceptance conditions:

- [ ] Provision only immutable Service Catalog entries (`postgres`, `redis`)
  on a Job/attempt-scoped rootless containerd network. No user-provided image,
  command, mount, port mapping, or host socket is accepted.
- [ ] Keep service storage ephemeral and disjoint across Job attempts; derive
  container/network names from the immutable Workload identity and remove them
  on success, failure, cancellation, timeout, startup error, and recovery.
- [ ] Enforce each service's catalog memory/CPU limits at container runtime
  and enforce the aggregate Job budget for commands plus sidecars. Reject a
  plan when its complete resource request exceeds the selected Lambda MicroVM
  profile; current manifest validation only checks the declared pipeline
  budget and does not enforce that budget against the MicroVM.
- [ ] Bind service ports only to the MicroVM loopback interface. Permit only
  the registry traffic needed to obtain pinned catalog images; do not provide
  arbitrary Internet or AWS access to the Job or its service containers.
- [ ] Generate PostgreSQL credentials per attempt, pass them only to the
  service and test process, and include them in the bounded log redaction set.
  Never persist them in DSQL, journal, result, artifact, or container labels.
- [ ] Wait for catalog health checks before executing pipeline commands and
  always perform bounded teardown. A teardown failure must keep the Workload
  non-terminal/cleanup-pending until the Resource Plane confirms removal.
- [ ] Add fake-backed lifecycle tests plus a local container integration test
  covering startup, health failure, command failure, cancel/timeout, crash
  recovery, resource limits, port isolation, credential/log redaction, and
  repeated cleanup. Keep Agent scheduling disabled until its separate
  executor, credential policy, and network/resource gates pass.

## Repository write policy groundwork (2026-10-24)

- Added a Tenant Repository Agent write policy for branch prefixes, optional
  same-repository PR creation, and required passing Hako Checks. The pure
  `internal/agenttask` authorization functions reject existing-branch updates,
  default/protected branch writes, unsafe branch paths, fork heads, non-default
  PR bases, missing Checks, and disabled policy.
- Added Owner/Admin API and `hako github repository agent-policy set` with a
  strict JSON policy file, bounded audit metadata, and a PostgreSQL migration.
  Configuration requires an active, Factory-enabled Tenant Repository; turning
  the Factory off clears the policy. The API explicitly returns
  `execution_enabled: false`, because the isolated Agent executor, model
  credential handoff, and runtime network/resource enforcement are not yet
  available. This does not grant GitHub write credentials or schedule Agent
  Runs.
- Unit/client/CLI tests cover policy scope and malformed input. A PostgreSQL
  Owner/Admin integration test verifies dormant configuration, cross-Tenant
  denial, Factory prerequisite, policy clearing, and transactional audit.
