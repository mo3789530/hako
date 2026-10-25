# Task 53: Native Pipeline and Issue-to-Agent Workflow

Status: In Progress (versioned manifest selection, fixed-catalog execution, durable per-pipeline results, and independent pipeline Checks through the existing Checks consumer are implemented and locally verified. Issue command authorization requires a verified GitHub identity that is a member of the resolved Tenant, an active Installation binding, exact repository identity, and a pre-existing Factory opt-in. A Tenant-App-only read scope, bounded Issue/default-branch SHA reader, Control Plane-only mTLS broker, and an atomic Issue inbox processor that creates one immutable Agent Run plus a bounded task record are locally composed and PostgreSQL-tested. The Resource Plane Worker retrieves the bounded task over mTLS and validates current attempt and immutable Run/App/Repository/branch/commit pins. A context digest is bound from launch specification through signed Start/Source tickets and guest `/run`; guest checks it before exposing task text in memory to a process factory. Purpose-scoped `native_agent` model credential metadata/API/CLI, a per-Repository default-off gate, an attempt/provenance-fenced in-process resolver, and an allowlisted Resource Plane mTLS Broker endpoint/client are implemented. The Worker now resolves model credentials after validating the immutable Issue task and sends them only in a no-store, attempt-ticket-bound HTTPS Start request to the allocated MicroVM. The Guest forwards the byte buffer only to an explicitly credential-capable process factory, clears request buffers, and redacts the credential from bounded output; the default Go-test factory remains incapable and fails closed. Linux Job command processes now use fail-closed cgroup v2 CPU/memory/PID scopes when launched as the fixed non-root identity; pipeline command budgets reserve fixed Service catalog requests first. Target Lambda MicroVM cgroup delegation/effective limits and egress remain unverified, so Agent scheduling and execution stay disabled pending a production isolated model executor and target network/resource validation. Service container orchestration is local and opt-in; live Agent write execution remains unavailable, and branch/PR policy is dormant configuration. Priority: P3.

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
  when task retrieval or pin validation fails. Agent dispatch remains disabled unless the separate model-access policy and runtime gates are satisfied.
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

## Local Agent executor progress (2026-10-25)

- Added an injectable `NativeAgentProcessFactory` with a bounded Anthropic tool
  loop. Its only tools are repository file listing/reading, constrained atomic
  file writes, and a fixed offline `go test ./...` verifier. It has no generic
  shell, GitHub, AWS, or arbitrary network tool. Paths reject traversal,
  symlinks, credential-like files/content, and protected policy/infrastructure
  locations; edit counts and bytes, turns, tool calls, file sizes, and test
  resources are bounded. A Run with edits cannot finish successfully without a
  successful test pass.
- Scripted model tests cover write → fixed tests → successful completion,
  untested-write rejection, and path/secret/symlink protections. Tests use
  synthetic credentials only. The executor is not selected by the production
  `hako-agent` composition; Agent scheduling therefore remains disabled.
  Branch/PR creation, target MicroVM egress/cgroup proof, and production image
  wiring remain incomplete.

## Agent proposal artifact progress (2026-10-25)

- Successful edits now produce a deterministic ZIP containing only bounded
  files changed by the Native Agent. The artifact is held in Guest memory until
  the Resource Plane fetches it using a separate short-lived, signed
  `changes` ticket bound to the same Agent execution, Tenant, Run, attempt,
  generation, Plane, immutable commit, and task-context digest. Failed and
  cancelled runs do not publish the bundle.
- The Resource Plane verifies the response length and SHA-256, writes it to the
  exact attempt's `agent-changes/` S3 key with SSE-S3 or configured SSE-KMS, and
  includes metadata only in the signed Workload result. Control Plane records
  kind `agent_changes` with migration 000176; the existing artifact download
  path serves the bytes. The proposal is explicitly untrusted and grants no
  branch/PR token. Unit coverage spans archive generation, ticket/API fencing,
  controller upload composition, storage scope, and result validation; the
  PostgreSQL migration 000176 and metadata persistence integration passed against
  the local database on 2026-10-25. Production IAM/deployment, MicroVM behavior,
  branch/PR creation, and human-review workflow remain incomplete.

## Agent proposal inspection (2026-10-25)

- Added `hako workload changes inspect <archive.zip>` as a local review aid. It
  never extracts or applies proposal data and prints only the verified archive
  SHA-256 plus sorted file paths and sizes. The shared archive validator rejects
  traversal/absolute/non-canonical paths, duplicate entries, symlinks and other
  non-regular entries, protected or secret paths, credential-like content,
  malformed ZIPs, and file/count/archive size violations. Unit and CLI tests
  cover the rejection boundaries. This does not create a Git branch or Pull
  Request and does not enable Agent scheduling or GitHub write credentials.

## Guest process composition (2026-10-25)

- `cmd/hako-agent` now composes the bounded Native Agent model loop and fixed
  offline Go test verifier only when the image/operator explicitly sets
  `HAKO_NATIVE_AGENT_ENABLED=true` and a supported fixed
  `HAKO_NATIVE_AGENT_MODEL`. The default remains disabled and the normal
  process factory does not accept Agent model credentials. The model endpoint
  remains the built-in Anthropic HTTPS endpoint; no endpoint override or proxy
  is accepted. The verifier runs as UID/GID 65532 with a required cgroup
  resource limiter.
- Unit tests verify default-off behavior, invalid configuration rejection,
  required model selection, and composition with the fixed test runner. This
  does not enable Control Plane scheduling or prove Lambda MicroVM cgroup,
  network egress, target ARM64 image, or model API connectivity. Keep Agent
  scheduling disabled until those target-runtime gates and a reviewed image
  deployment pass.

## ARM64 image build attempt (2026-10-25)

- Reviewed official release assets before building: `nerdctl-full` v2.4.1
  Linux ARM64 archive SHA-256
  `2deffe14625f93866f90a8f265d0f59d6fa476c7dc54b98adb327b1b4613efd4` and
  `slirp4netns` v1.3.6 AArch64 binary SHA-256
  `045f85dd0bc51a61b99cdfe6bd22282cbc308b73caa64fc8859c11d3775c5817` matched
  the official GitHub release asset digests. The archive contains the required
  `nerdctl`, `containerd`, rootless launcher, and RootlessKit binaries. The
  Go Agent cross-compiles to a static ARM64 executable.
- `make build-workload-microvm-services-image` reached the Lambda MicroVM base
  image's first ARM64 `RUN` instruction, then failed with `Exec format error`:
  this x86_64 host has no `qemu-aarch64`/binfmt handler. The final services
  image was not built. ARM64 Podman image targets now preflight the host and
  fail early with this prerequisite instead of pulling layers before surfacing
  the same limitation. Do not treat artifact digest review or Go cross
  compilation as image/runtime validation; repeat the build on ARM64 or a
  configured emulation host, then separately validate inside Lambda MicroVM.
- Follow-up: both Workspace and Workload service image targets now require an
  explicit `HAKO_CNI_PLUGINS_VERSION`, verify that it matches the embedded
  nerdctl-full manifest and is at least 1.1.0, and require the bundled CNI
  `firewall` plugin before building. This guards the `--internal` network
  contract at image construction time; ARM64 image execution and MicroVM
  egress behavior remain unverified.
- Sources: [nerdctl v2.4.1 release](https://github.com/containerd/nerdctl/releases/tag/v2.4.1)
  and [slirp4netns v1.3.6 release](https://github.com/rootless-containers/slirp4netns/releases/tag/v1.3.6).

## Local verification refresh (2026-10-24)

- Added a Job/attempt-scoped `CatalogJobServiceManager` contract and connected
  it to the native Pipeline process lifecycle. Each Pipeline provisions only
  the built-in `postgres`/`redis` catalog on a deterministic hashed scope,
  waits for the catalog health check, exposes fixed loopback connection
  variables to its commands, and removes the services/network with a separate
  bounded cleanup context after success, failure, timeout, or cancellation.
  Generated PostgreSQL credentials are retained only for that process attempt
  so the final bounded log buffer can be redacted; cleanup failures fail the
  Pipeline and remain retryable. Fake-engine tests cover ephemeral scope,
  health failure cleanup, idempotent close/retry, command environment, cleanup
  failure, and credential redaction.
- Added a separate Linux `NerdctlJobServiceEngine`. It uses a dedicated
  containerd namespace, `--pull=never`, catalog-only digest images, an
  `--internal` network, fixed `999:999` service user, read-only rootfs, dropped
  capabilities, no-new-privileges, CPU/memory/PID limits, loopback-only port
  publishing, and bounded tmpfs for all database state. Container and network
  labels are checked before removal; unknown ownership fails closed. Secrets
  enter only through a mode-0600 environment file in a verified tmpfs and the
  file is removed after container creation. Fake runner tests verify command
  construction and foreign-resource refusal. A local Podman smoke run using
  the available x86_64 PostgreSQL 18 image verified startup, `pg_isready`, and
  `127.0.0.1` port binding with the same core restrictions. This is not a
  runtime test of the pinned ARM64 images or nerdctl itself.
- Added repeatable `make test-job-services-podman` coverage that drives the
  production Job Service engine through an integration-test-only Podman CLI
  adapter. It verifies PostgreSQL startup/health, generated credential
  handoff, tmpfs data, 1 GiB/0.75 CPU limits, read-only rootfs, loopback-only
  port publishing, credential env-file removal, and cleanup. The host port is
  remapped to an ephemeral loopback port because `hako-dev-postgres` keeps
  port 5432 occupied; the development database is not stopped. This exercises
  Podman on the host, not nerdctl/cgroup enforcement in Lambda MicroVM.
- The Linux Job Service Engine now inspects each created container and fails
  closed unless its observed memory, NanoCPU, PID limit, read-only rootfs,
  non-privileged state, no added capabilities, `no-new-privileges`, no bind
  mounts, `999:999` user, exact loopback port bindings, and catalog tmpfs
  paths/options match the fixed template. The tmpfs verifier permits only
  Podman's known `rprivate` and `tmpcopyup` additions. Unit tests reject
  mismatched memory limits, privileged mode, added capabilities, writable
  rootfs, missing `no-new-privileges`, bind mounts, and public port bindings;
  each failed container is verified removed. The Podman acceptance exercises
  the inspect contract. This confirms configured
  container metadata on host Podman; it does not prove kernel/cgroup enforcement
  or validate nerdctl inside Lambda MicroVM.
- Extended that Podman integration to build a connection from the exact
  per-attempt `DATABASE_URL`/`PGPASSWORD` returned to the Job process, connect
  through the ephemeral loopback port, authenticate to PostgreSQL, and execute
  `SELECT 1`. The fixture does not print the generated credential or provider
  error text. This verifies the local handoff and port path, not target
  MicroVM/nerdctl enforcement.
- Extended the Podman acceptance to read `memory.max`, `cpu.max`, and
  `pids.max` from the running PostgreSQL container's cgroup v2 filesystem. The
  host Podman acceptance passed with the expected 1 GiB, 0.75 CPU, and 256 PID
  limits. This verifies local cgroup v2 application, not the pinned ARM64
  nerdctl runtime or Lambda MicroVM kernel.
- Extended the explicit Podman acceptance to check external egress from the
  running Service container whenever the host can reach the same fixed
  `1.1.1.1:443` baseline. The Service uses the engine's labeled internal
  network; a successful container connection fails the test. The last local
  run could not reach the host baseline and correctly skipped this assertion;
  the CI/host test can exercise it when external TCP egress is available. This
  is intended as host Podman evidence only; it does not prove
  nftables/iptables, mapped UID, or Lambda MicroVM egress enforcement.
- The guest resolver admits Service declarations only when image-owned
  configuration explicitly enables the Job Service manager; its default
  constructor and the default image remain disabled. Rootless containerd is
  available only in a dedicated opt-in image target and must be exercised in
  the target MicroVM. The runtime socket stays owned by UID
  100000, distinct from the Job UID; target nerdctl label inspection, pinned
  ARM64 image behavior, host firewall support, and container egress need
  execution tests. Those conditions keep Service pipelines and Agent
  scheduling disabled.

- Added a Linux Job egress guard to the immutable workload image. Before the
  Agent opens its listener, and again before each Job process starts, it
  installs IPv4 and IPv6 OUTPUT rules that allow loopback and reject other
  traffic from UID 65532, the containerd daemon UID, and its configured mapped
  UID range. Startup and Job execution fail closed when either firewall family
  cannot be configured. This is local code evidence; target Lambda MicroVM
  netfilter support and mapped-user traffic still require runtime validation.

- Tenant model credentials now have an explicit `claude_action` or
  `native_agent` purpose in both the operator-approved binding and Tenant
  credential row. The Owner/Admin API and CLI create, list, rotate, and disable
  credentials independently by purpose; migrations 000148–000153 preserve
  existing rows as `claude_action`. PostgreSQL integration verifies
  cross-purpose binding rejection and that rotating/disabling one purpose does
  not change the other. Native Agent credential metadata remains inert: its
  existing Actions Runner broker resolver is purpose-pinned to `claude_action`,
  and Agent scheduling remains disabled until its independent runtime policy
  and executor are implemented.
- Added a default-off per-Repository `agent_model_access_enabled` gate. An
  Owner/Admin can enable it only when the Repository Factory is enabled and the
  Tenant has an active, approved Anthropic `native_agent` credential binding.
  Disabling Factory opt-in clears this gate, and disabling model access clears
  its configured timestamp. The API/CLI response explicitly reports
  `execution_enabled: false`; this gate does not schedule Agent Runs, resolve
  credentials for an Agent, or deliver Secret material. Migrations 000154–000155
  add the persisted gate and state/timestamp consistency constraint. API
  integration verifies missing-credential and cross-Tenant/Member denial,
  Owner enable/disable, timestamp consistency, Factory opt-out reset, and
  atomic audit events. API client and CLI parser tests cover the new endpoint
  and explicit boolean flag.
- Added an in-process Native Agent credential resolver that requires the exact
  latest active Agent attempt, calling Resource Plane, active Tenant GitHub
  integration and Installation, immutable Run/Issue delivery pins, current
  Tenant membership, Repository Factory opt-in, Repository model-access gate,
  and an approved `native_agent` Anthropic credential. The Tenant/Repository
  gate and credential must predate the Issue delivery. Authorization is checked
  again after SecretReader I/O and the byte buffer is cleared after the trusted
  consumer returns. PostgreSQL tests use only a synthetic reader and cover
  default-off denial, wrong attempt/Plane, policy disable, and credential
  rotation during read. An allowlisted Resource Plane mTLS Broker endpoint and
  bounded no-store HTTP client expose this credential only to the trusted
  Worker process; endpoint tests cover mTLS identity, strict request decoding,
  response bounds, and cache policy. A PostgreSQL integration test crosses the
  DB resolver, broker Service, and HTTP route with a synthetic SecretReader;
  it verifies the allowed Plane response and foreign-plane rejection. The Resource Plane Worker now composes this mTLS client into Agent dispatch.
  After source verification/upload it obtains the current attempt credential
  and sends it through the MicroVM authenticated Start endpoint. The guest
  passes it only to a `NativeAgentCredentialProcessFactory`, rejects generic
  factories, clears request/launch buffers, redacts completed output, and
  clears its in-memory copy on terminal completion. Worker, Guest, and HTTP
  unit tests use synthetic values; they do not call AWS or retrieve a real
  Secret. No production Native Agent executor is configured, so Agent
  scheduling and execution remain disabled.

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
  work until the production isolated model executor and runtime resource/network
  policy are implemented. The Tenant credential policy and local handoff
  primitives exist, but no AWS apply or real Secret read was performed.
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
- Resource Plane Job Runtime Profiles now declare allocatable memory and CPU
  ceilings. The selected profile values are frozen into the launch
  specification, Lambda `/run` identity, and signed Start/Source tickets.
  Guest manifest resolution rejects a complete pipeline request above either
  ceiling before process start. Unit tests cover profile validation, signed
  propagation, and over-capacity rejection. These configured ceilings do not
  prove that Lambda MicroVM cgroups enforce them; target runtime validation is
  still required.
- The guest resolver admits a valid `postgres` sidecar plan only through an
  explicit Job Service configuration; default admission remains fail-closed.
  The Agent enables this only when the image has composed the Job-scoped
  manager. This protects the boundary against accidental reuse of
  Workspace-owned Service IDs or volumes; it does not prove target enforcement.

## Remaining runtime implementation gates

The current Workspace Service Engine is not a Job service runtime: it uses a
Workspace identity, durable Service IDs/volumes, and Workspace lifecycle
commands. The default Workload Agent image does not include or start the
rootless containerd toolchain. An opt-in image build target and daemon
supervisor now exist, but the target has not been built with the reviewed
ARM64 artifact or run inside Lambda MicroVM. Do not enable manifest `services`
by reusing the Workspace path.
The Job runtime needs its own implementation with these acceptance conditions:

The local `NerdctlJobServiceEngine` now passes the pinned nerdctl
`--cgroup-manager=systemd` setting on every command. Nerdctl documents that
rootless CPU/memory limits require systemd and cgroup v2
([v2.4.1 rootless requirements](https://github.com/containerd/nerdctl/blob/v2.4.1/docs/rootless.md)).
If the Lambda MicroVM image cannot provide that manager/delegation, service
creation fails instead of relying on silently ignored limits. This command
contract is unit-tested; target MicroVM enforcement remains unverified. Service
admission is default-off and must remain disabled in production until those
target gates pass.

- [x] Provision only immutable Service Catalog entries (`postgres`, `redis`)
  on a Job/attempt-scoped rootless containerd network. No user-provided image,
  command, mount, port mapping, or host socket is accepted.
- [x] Keep service storage ephemeral and disjoint across Job attempts; derive
  container/network names from the immutable Workload identity and remove them
  on success, failure, cancellation, timeout, startup error, and recovery.
- [x] Reject a Factory pipeline whose aggregate declared CPU or memory budget
  exceeds the selected Resource Plane Job profile's allocatable ceiling. The
  trusted ceiling is frozen into the launch specification, MicroVM `/run`
  binding, and signed Agent tickets before Guest manifest evaluation.
- [x] Apply catalog Service limits and enforce aggregate declared Job
  CPU/memory for commands plus sidecars: the fixed services use their catalog
  cgroups, and Linux non-root command processes receive the pipeline budget
  remaining after those reservations. The selected profile ceiling is checked
  before scheduling. Target Lambda MicroVM cgroup delegation/effective-limit
  validation remains an operational gate.
- [x] Bind catalog service ports only to `127.0.0.1`. Each Job attempt uses an
  internal container network, and service creation uses `--pull=never`, so
  repository-controlled processes and sidecars cannot fetch images at runtime.
  The production engine's Podman acceptance test verified PostgreSQL remained
  reachable through loopback while a TCP connection to an external endpoint
  was denied (2026-10-25).
- [ ] Repeat the network and port-isolation acceptance on the pinned ARM64
  nerdctl image inside the Lambda MicroVM, including confirmation that its
  network connector cannot reach AWS endpoints or arbitrary Internet hosts.
  Podman on the development host does not prove the target connector,
  rootless-network, or MicroVM firewall behavior.
- [x] Generate PostgreSQL credentials per attempt, pass them only to the
  service and test process, and include them in the bounded log redaction set.
  Never persist them in DSQL, journal, result, artifact, or container labels.
- [x] Wait for catalog health checks before executing pipeline commands and
  always perform bounded teardown. The Resource Plane durably checkpoints the
  Agent's terminal result but withholds publication to the Control Plane until
  MicroVM cleanup is confirmed; cleanup errors retain the result and retryable
  cleanup state without making the Control Plane Workload terminal.
- [x] Add fake-backed lifecycle tests and a local Podman integration test
  covering startup, health failure, command failure, cancel/timeout, crash
  recovery, resource limits, port isolation, credential/log redaction, and
  repeated cleanup. The Podman test exercises the production Job Service engine
  through a narrow CLI compatibility adapter with the locally cached amd64
  PostgreSQL image; it is not evidence for nerdctl, the pinned ARM64 image, or
  the Lambda MicroVM. Keep Agent scheduling disabled until its separate
  executor, credential policy, and network/resource gates pass.

The checked items above have local implementations and tests. Podman confirms
effective cgroup v2 memory/CPU/PID values for Service containers, while Linux
command processes receive an attempt-scoped cgroup with the remaining pipeline
budget. The current host does not permit creating a cgroup under
`/sys/fs/cgroup`, so tests cover budget calculation and fail-closed behavior,
not effective command-process kernel limits. Target ARM64 nerdctl, Lambda
MicroVM delegation/enforcement, container egress, and cleanup confirmation
remain unverified; Service and Agent production admission must remain disabled
until those gates pass.

## Linux Job command cgroup limits (2026-10-25)

- Added `LinuxJobCgroupManager` under the Workload Agent. For the fixed
  non-root Job command identity it requires a cgroup v2 root with `cpu`,
  `memory`, and `pids` enabled, creates a unique attempt/process scope, applies
  `memory.max`, `cpu.max`, and `pids.max`, and starts the process atomically
  inside that cgroup with `SysProcAttr.UseCgroupFD`. Cancellation/finish kills
  remaining descendants and removes the cgroup before returning terminal
  completion. Missing controllers, writes, or cleanup fail closed.

## Terminal result and Job Service cleanup ordering (2026-10-25)

The Resource Plane saves a terminal Agent result in its journal, confirms
Lambda MicroVM termination, and only then publishes the result to the Control
Plane. If cleanup fails, the result remains durable and unpublished while the
journal stays cleanup-pending/failed; a later Poll retries provider cleanup
without recollecting the Agent result. This also covers Job Service teardown
failure: the Control Plane Workload remains nonterminal until the attempt
MicroVM, and therefore its sidecar containers, is confirmed removed.
PostgreSQL/fake-runtime tests cover cleanup failure, withheld publication,
retry, and successful result publication. This is local lifecycle evidence;
deployed recovery scheduling and Lambda MicroVM termination remain unverified.
- Pipeline process limits subtract the immutable catalog CPU/memory requests
  for declared Service containers. Service containers keep their own fixed
  catalog limits, making the requested CPU and memory caps sum to the pipeline
  budget. This is local Linux implementation evidence, not proof that the
  Lambda MicroVM exposes a writable/delegated cgroup v2 hierarchy.
- The `hako-agent` image composition uses `HAKO_JOB_CGROUP_ROOT` or
  `/sys/fs/cgroup`; no cgroup is created during startup. The host used for
  these tests has a read-only cgroup mount for this user, so effective kernel
  enforcement remains a target-MicroVM gate. Agent scheduling stays disabled.
- `go test -p=1 ./internal/workloadagent ./cmd/hako-agent ./internal/workloadcontroller -count=1` passes. Tests cover catalog-budget subtraction, invalid budgets, cgroup event parsing, and rejection when a usable cgroup v2 hierarchy is absent.

## Process identity isolation (2026-10-24)

- The workload image now runs the trusted `hako-agent` as root while fixed
  Go-test and Native catalog child commands are launched as UID/GID
  `65532:65532` with no supplementary groups. Linux tests verify identity
  validation and, when the test process is root, the actual child UID. This
  establishes a process-identity boundary for the rootless containerd socket.
  The default image does not start that runtime, so Service declarations
  remain rejected and Agent scheduling remains disabled.
- Added a dedicated Linux Job rootless-containerd supervisor and opt-in
  `Dockerfile.services`/`make build-workload-microvm-services-image` path. The
  supervisor requires the exact runtime UID/subuid layout, setuid mapping
  helpers, tmpfs runtime path, root-owned secret-file directory, and a private
  socket owned by UID 100000. The daemon is started only when
  `HAKO_JOB_SERVICES_ENABLED=true`; the image default is false. The manager and
  image target have not yet been exercised with the reviewed ARM64 nerdctl
  archive or an actual Lambda MicroVM. Manifest Services remain rejected by
  the trusted resolver because Job egress and resource-limit enforcement are
  not proven. Upstream nerdctl documents memory limits as requiring systemd
  plus cgroup v2; neither capability has been demonstrated in the target
  Lambda MicroVM, so command construction tests are not treated as proof of
  enforcement.

## Repository write policy groundwork (2026-10-24)

- Added a Tenant Repository Agent write policy for branch prefixes, optional
  same-repository PR creation, and required passing Hako Checks. The pure
  `internal/agenttask` authorization functions reject existing-branch updates,
  default/protected branch writes, unsafe branch paths, fork heads, non-default
  PR bases, missing Checks, and disabled policy. Branch and prefix validation
  also rejects Git-invalid path components such as dot-prefixed components,
  trailing dots, `.lock` suffixes, empty components, and traversal sequences.
- Added Owner/Admin API and `hako github repository agent-policy set` with a
  strict JSON policy file, bounded audit metadata, and a PostgreSQL migration.
  Configuration requires an active, Factory-enabled Tenant Repository; turning
  the Factory off clears the policy. The API explicitly returns
  `execution_enabled: false`, because the production isolated Agent executor
  and target runtime network/resource enforcement are not yet available. The
  local model credential handoff is inert unless an authorized Agent Run is
  dispatched and a credential-capable Guest factory is configured. This does not grant GitHub write credentials or schedule Agent
  Runs.
- Unit/client/CLI tests cover policy scope and malformed input. A PostgreSQL
  Owner/Admin integration test verifies dormant configuration, cross-Tenant
  denial, Factory prerequisite, policy clearing, and transactional audit.
- A CLI-to-local-API test now exercises branch/PR write-policy upload and the
  Repository model-access toggle. It asserts exact Tenant/Installation/
  Repository routing and that both API responses keep `execution_enabled`
  false; malformed policy JSON with an unknown field is rejected before any
  HTTP request. This remains configuration-only and does not schedule an Agent.

## Model credential resolver guard coverage (2026-10-24)

- Added unit coverage for nil resolver/context/consumer rejection on both
  Actions Runner and Native Agent credential APIs, proving invalid calls do
  not reach the Secret reader. Printable-ASCII and 8 KiB value bounds are now
  tested at empty, exact-limit, oversized, control-byte, and non-ASCII edges.
  `go test ./internal/modelcredentialbroker -cover` reports 51.5% statement
  coverage. PostgreSQL provenance/rotation behavior remains in the integration
  suite; Agent scheduling and production execution remain disabled.

## Native Agent credential handoff (2026-10-25)

- Wired the Resource Plane Worker to the Tenant-scoped model-credential mTLS
  client. A credential is requested only after the exact Agent task pins have
  been validated and source upload succeeds, then sent to the allocated
  MicroVM through its no-store HTTPS Start route and signed attempt ticket.
- The Guest protocol bounds credentials to 8 KiB and rejects control bytes.
  Its default Go-test factory cannot consume the key and fails Agent execution
  closed. A factory must explicitly implement
  `NativeAgentCredentialProcessFactory`; its Start call receives the transient
  bytes, which are cleared on return. The backend retains a temporary copy only
  for output redaction, then clears it when the process reaches a terminal
  state. HTTP request/response buffers and bounded output buffers are cleared.
- Added tests for Worker identity/payload/clear behavior, credential-capable
  factory handoff, rejection by a generic factory, and terminal log redaction.
  `go test -p=1 ./... -count=1` passed. The tests use synthetic values; no AWS
  Secret was read. No production model executor is configured, so Agent
  dispatch remains disabled.

## Required Podman acceptance in coverage and CI (2026-10-25)

The Task 53 rootless Job Service test was previously opt-in and therefore
skipped by the generic CI integration command. CI now pulls the pinned local
PostgreSQL service image and sets `HAKO_JOB_SERVICES_PODMAN_TEST=1`. The
repository-wide `make test-coverage` target also requires both Task 42 and
Task 53 Podman acceptances instead of silently skipping them. The focused
Job Service test and Task 42 Software Factory Podman/PostgreSQL E2E pass
locally; the coverage run reports 70.5% aggregate statement coverage. These
host container tests do not establish Lambda MicroVM, ARM64 nerdctl, or AWS
network isolation behavior.

## Authorized Run artifact metadata (2026-10-25)

- Authenticated Workload Run detail now includes bounded artifact metadata for
  the authorized requester or Tenant Owner/Admin. It omits the internal S3
  storage reference; `hako workload show` decodes the metadata through the
  existing Run endpoint. PostgreSQL integration coverage verifies the
  metadata and proves the backing storage URI is not exposed. General artifact
  byte retrieval is now implemented by [Task 87](task-87-workload-artifact-download.md).
  Agent-generated change artifacts now use the bounded attempt-scoped
  `agent_changes` bundle described below. GitHub branch/PR submission remains
  unimplemented.

## Native Agent model transport foundation (2026-10-25)

- Added a bounded Anthropic Messages API transport used by the injectable Native Agent
  executor. It pins requests to `https://api.anthropic.com/v1/messages`, uses
  TLS 1.2+, disables proxy discovery and redirects, requires an operator-chosen
  model name, and rejects oversized or malformed messages/tool blocks. Provider
  response bodies and credentials are omitted from returned errors.
- Tests use a synthetic key and an in-process HTTP doer to verify the fixed
  endpoint, authentication/version headers, client-tool response parsing,
  malformed tool rejection, request-model pinning, and provider-error
  redaction. No real API request or credential retrieval was performed.
- The transport is consumed by the injectable fixed-tool process factory and
  its scripted tests. It makes no live API request. No production Guest
  composition uses the factory; Agent scheduling remains disabled until image
  wiring and target MicroVM resource/network controls are validated. The
  attempt-scoped proposal bundle is implemented in the following section;
  branch/PR submission remains unavailable.
