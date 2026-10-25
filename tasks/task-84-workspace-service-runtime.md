# Task 84: Workspace Service runtime reconciliation

Status: In Progress (service instances now carry a monotonic desired revision
and an observed revision in the domain model and DSQL schema. API responses
expose both revisions; state transitions increment desired revision while
idempotent repeated requests preserve it. PostgreSQL integration tests verify
initial and transitioned revisions. Service create/state changes now append a
non-secret desired-state snapshot to the Outbox in the same transaction as
authorization and audit. Dispatcher routes only to a distinct optional
per-Resource-Plane Service queue from the validated manifest; absent routes
stay unpublished and retry. A local Resource Plane queue consumer and
HMAC-authenticated Control Plane report endpoint now
accepts bounded, replay-protected reports and updates observed state only for
the current desired revision and exact active Lambda MicroVM binding.
PostgreSQL integration coverage verifies valid, duplicate, conflicting-ID,
foreign-MicroVM, and stale-revision reports. A local Resource Plane consumer now resolves the
current running allocation from the durable journal, revalidates the provider
MicroVM/image/network binding, requests a two-minute AWS token for port 8080
only, issues the one-use Ed25519 Guest capability, invokes the fixed Guest
route, and reports the result through the HMAC sink. Its SQS Lambda adapter
returns per-record failures for retry. An AWS Lambda executable now composes
the consumer from the S3 journal, Lambda MicroVM API, mounted Ed25519 key, and
report HMAC key from two distinct exact-ARN Secrets Manager references; it
retrieves them at Lambda cold start, while local tests use fakes only. ARM64 ZIP
packaging is exposed by `make build-workspace-service-controller-lambda`.
Resource Plane Terraform now has default-off queue/DLQ, exact Dispatcher send
policy, private ARM64 Lambda, queue event mapping, scoped runtime IAM, alarms,
nullable outputs, and dev-root inputs. Six OpenTofu mock plan tests and dev-root
validation pass; network endpoints/routes, coordinated key-rotation rollout,
and live AWS deployment remain unimplemented. Cross-MicroVM cleanup now has a
durable generation fence described below.
Before Guest dispatch, the Lambda consumer now uses a timestamped
HMAC-authenticated Control Plane endpoint to resolve the latest desired
Service command by exact Tenant/Workspace/Plane/Service identity. Old SQS
payloads are wake-up signals only; tests verify that a stale queued start
resolves to a newer stop. PostgreSQL API integration tests verify latest
revision lookup, invalid HMAC rejection, and hidden cross-Tenant lookup.
This closes stale replay after MicroVM replacement. After Guest apply, the
consumer resolves desired state again; if a newer revision arrived during the
side effect, it withholds the old report and fails the SQS record for retry, so
the retry applies the latest state. This provides retry-driven convergence but
does not make the Guest side effect and Control Plane mutation strongly
mutually exclusive; a short transient old state remains possible.
A local injectable Agent-side
reconciler verifies exact built-in catalog JSON and fences same-process
revision reordering/replay. A Resource Plane Secrets Manager provider now
generates a random stable per-Tenant/Workspace/Service credential, verifies
name and ownership tags on retries, and returns only catalog-declared values.
The Lambda consumer passes values only in the authenticated request to the
currently bound Guest; the Guest validates the exact generated variable set.
The rootless `nerdctl` adapter supplies values via a mode-0600 temporary env
file under private `/run/user/{uid}` and removes it after container creation.
Fake/local tests cover this flow; no real Secrets Manager calls were made.
Explicit credential rotation advances a separately persisted credential
generation and the Service desired revision in one authorized API transaction.
The Resource Plane stores deterministic generation-specific `AWSPENDING`
versions, the Guest applies the fixed PostgreSQL role update through `psql`
stdin, and the controller promotes `AWSCURRENT` only after a healthy Guest
result plus desired-state and MicroVM-generation revalidation. Replays reuse
the same staged value; restart reapplies the current credential to the existing
volume. Fake Secrets Manager and Guest tests cover staging, retry, promotion,
API/Outbox/audit, capability binding, and stdin handling. No real Secret was
retrieved and real MicroVM rotation remains unverified. After the currently
bound Guest confirms Service removal and the desired-state generation is revalidated, the controller now
rechecks exact ownership tags and schedules generated Secrets Manager
credentials for deletion with a seven-day recovery window. Missing/already
scheduled secrets are idempotent; foreign-tagged secrets are rejected, and a
failed retirement withholds the deleted report for retry. The Terraform role
has an opt-in `DeleteSecret` permission limited to the Hako Service secret path
and the `hako:managed-by=hako` resource tag; mock-provider tests cover this
scope. No real secret was read or deleted. A separate one-use signed capability binds the
Tenant/Workspace/Plane/MicroVM/Service/revision/state/catalog/config digest and
the private Workspace Agent can optionally expose its bounded HTTP handler.
The root init verifies each Service capability and persists capability replay
IDs and the highest accepted revision atomically in its private `/run` state
before proxying; this survives Agent child restarts within one MicroVM and is
reset when a new MicroVM identity is bound. The Workspace init image now has a
default-off build-time opt-in that configures the per-MicroVM passwd/subuid
identity, starts rootless containerd under the Workspace UID, supervises it
independently from the restartable Agent child, and passes the Agent only the
fixed containerd socket path. The supervisor rejects any non-fixed daemon
socket and stops a daemon returned with an invalid socket, avoiding a leaked
process or arbitrary socket handoff. Workspace Agent race tests, the full Go
suite, PostgreSQL integration suite, and ARM64 init/agent cross-build pass;
they do not replace a real Lambda MicroVM runtime PoC. Verified upstream
`nerdctl-full` ARM64 archives do not bundle `slirp4netns`; the image build now
accepts a separate static ARM64 binary pinned by version and SHA-256, rather
than rejecting the official archive or assuming the distro package exists.
The same correction is applied to the Job image target. Runtime setup uses
ephemeral `/tmp` state rather than EFS. The Lambda executable and Terraform SQS
event-source/IAM wiring exist, but deployment remains default-off and unapplied. An optional
rootless `nerdctl` adapter now runs only exact built-in templates, applies
memory/CPU limits, maps service ports to guest loopback, uses a Workspace
private network and Workspace-scoped volume paths, and strips ambient process
environment; it is fake-runner tested. The image build accepts only a
caller-pinned, SHA-256- and version-checked `nerdctl-full` ARM64 bundle and a
separately SHA-256-pinned static ARM64 `slirp4netns` binary, plus rootless UID
map helpers. With the default-off
`HAKO_WORKSPACE_SERVICES_ENABLED=true` build setting, root init configures the
per-MicroVM account/subuid mapping, supervises rootless containerd under the
Workspace UID, and passes the Agent child only its fixed socket path. This
lifecycle is locally tested, but real MicroVM container startup, user
namespace/network behavior, cgroup limits, and image compatibility remain
unverified. Generated PostgreSQL credential delivery, generation-fenced
rotation, and post-removal seven-day recovery-window retirement are locally
covered; real MicroVM execution remains open. Task 47 currently
persists authenticated Service Catalog desired state and displays desired /
observed state, revision, and the runtime generation backing each health
report. Its API list masks a prior generation's running status as pending until
the current active MicroVM reports for the current revision; the CLI reports
health only when that binding is present. These paths are locally tested, but
deployed MicroVM behavior remains unverified. `remove` now removes the container before
the Service-ID-scoped data subtree, uses descriptor-relative no-follow
filesystem operations, and returns failure on cleanup errors so the
Control Plane can retain its tombstone. Tests cover successful data cleanup
and refusing a symlinked service directory without touching its target.
Cross-MicroVM generation-fenced cleanup confirmation is implemented and covered
by PostgreSQL integration tests for generation advancement, stale reports,
prior-generation retirement, Workspace deletion, and rejected cleanup retry.
This task connects the
transactional Control Plane request to a Resource Plane command and a fixed,
private Workspace MicroVM executor. Lambda MicroVM is the only execution
substrate; EC2 is out of scope. No AWS apply or external cutover is included.
Priority: P1. Parent: Task 47. Depends on: Tasks 40, 43–46, 47, 80–83.

Workspace Service networking now creates its per-Workspace nerdctl network with
`--internal` and inspects an existing network before reuse. Missing, malformed,
or externally connected network metadata fails closed before container start.
The pinned nerdctl-full bundle must provide CNI plugins 1.1.0 or later for the
isolation behavior. This is container-level Service egress control only; it is
not proof of Lambda MicroVM network isolation or the production image.

Local verification entrypoint: `make test-workspace-service-runtime-integration`
runs the Guest/Controller package tests and the PostgreSQL API lifecycle test
for authorization, desired-state revisions, transactional Outbox delivery,
signed reports, stale-report fencing, and credential rotation. This does not
exercise the Workspace `NerdctlServiceEngine` against Podman inside a Lambda
MicroVM. The existing `make test-job-services-podman` targets the separate
Workload Job Service adapter and is not evidence for Workspace Service runtime
behavior. Real ARM64 image/containerd execution and MicroVM isolation remain
unverified.

## Focused local verification refresh (2026-10-25)

`make test-workspace-service-runtime-integration` passes on the local host.
It runs `internal/workspaceagent` and `internal/workspaceruntime` unit tests,
then the PostgreSQL-backed API lifecycle test
`TestTenantMembershipRouteResolvesUserAndRejectsNonMembers`, which exercises
Workspace Service admission, authorization, transactional command Outbox,
revision transitions, signed Resource Plane reports, stale-report rejection,
and credential rotation. The always-on `hako-dev-postgres` container remained
running. `make test-job-services-podman` also passes, but exercises the distinct
Workload Job adapter; it does not close the Workspace MicroVM runtime gate.

## Goal

Make `hako add`, `hako service start|stop|remove`, and `hako services` reflect
the real lifecycle of approved PostgreSQL and Redis templates inside the
selected Workspace MicroVM, while keeping container access private and
retries/reordered commands safe.

## Work

- Extend each Service with a monotonic desired revision and an observed
  generation. In the same PostgreSQL transaction as authorization, state
  update, and audit, append an outbox command containing only Tenant,
  Workspace, Service, Resource Plane, template version/config, desired state,
  and revision. Never include credential values.
- Route the command by the immutable Workspace Placement through the existing
  Control Plane Dispatcher/Resource Plane queue boundary. Resource Plane
  consumers must validate the schema, placement, Workspace/MicroVM binding,
  revision, and idempotency before invoking a guest capability. Missing queue
  or runtime configuration fails closed; do not mark the outbox event
  published until queue send succeeds.
- Add a one-use, short-lived, signed Guest Agent capability for Service
  reconciliation. It binds Tenant, Workspace, Resource Plane, MicroVM,
  Service ID, revision, operation, and approved catalog configuration. The
  guest must not accept arbitrary image, command, environment, host port,
  mount path, or network settings from a caller.
- Implement an injectable container-engine interface and a production
  containerd adapter in the Workspace MicroVM Agent. Resolve only validated,
  versioned catalog templates; enforce memory/CPU limits, fixed healthcheck
  argv, private Workspace networking, loopback-only port forwarding, and
  Workspace-scoped data volumes. Containerd endpoints and credentials stay
  inside the MicroVM and are never exposed by the Gateway.
- Define generated-secret handling for templates such as PostgreSQL without
  putting plaintext in DSQL, outbox/SQS, process arguments, image layers, EFS,
  logs, or artifacts. The selected secret reference/provider must support
  rotation and restart without returning secret values through the public
  Service API. Document its IAM and local-test boundary before enabling it.
- Report starting/running/healthy/unhealthy/stopped/failed/deleting outcomes
  with the exact Service revision. Reject stale reports and retain a
  tombstone until runtime cleanup is confirmed. `remove` must be idempotent,
  retryable, and must not release the name or data volume before cleanup
  succeeds.
- Keep development databases Workspace-local by default. A managed persistent
  database remains a separate explicit feature and must not be implied by the
  catalog `persistent` volume declaration.

## Acceptance criteria

- PostgreSQL integration tests prove API authorization, state/audit/outbox
  atomicity, stable idempotency, same-name conflicts, outbox retry, and
  cross-Tenant/Workspace rejection.
- Fake-engine and local Podman/containerd-compatible tests cover start,
  health, stop, restart, remove, duplicate/reordered revisions, timeout,
  engine errors, resource limits, image policy, and confirmed cleanup.
- An older Service revision cannot overwrite the latest observed state or
  restart a Service after a newer stop/remove request.
- Tests verify no public listener/host port, no arbitrary runtime command or
  image, no plaintext secret in persisted/event/log output, and no AWS
  credential in the MicroVM.
- Service removal deletes the runtime container and only that Service's
  Workspace-local data subtree; failures remain retryable and must not be
  reported as confirmed deletion. Control Plane reports are fenced by both
  active MicroVM ID and allocation generation; Resource Plane rechecks its
  durable Workspace journal after Guest side effects. Per-Service runtime
  generations remain durable until the old MicroVM lifecycle is confirmed
  terminated, and a deleted report is accepted only when no older generation
  remains active. A rejected report is retryable by the SQS consumer, while an
  identical report replay returns its persisted acceptance decision.
- `hako services` distinguishes desired and observed state and reports health
  only when a matching-generation runtime report exists. Documentation states
  exactly which health and persistence guarantees are provided.
- Focused tests, race tests, and `go test ./...` pass. Local PostgreSQL remains
  running. No real Secret retrieval, AWS apply, or external cutover occurs.

## Implementation order

1. Service revision, transactional outbox, and Resource Plane routing contract.
2. Signed Agent command and revision-fenced status report.
3. Container-engine abstraction and fixed-template containerd executor.
4. Secret reference/provider and ephemeral credential injection.
5. Cross-MicroVM generation-fenced cleanup confirmation.
6. API/CLI health semantics, local end-to-end tests, and rollout documentation.

## Generation-fenced cleanup implementation

Resource bindings persist the Lambda MicroVM allocation generation, and
operation-result projection only accepts monotonic generation updates. The
Control Plane records each Service's observed runtime generation and binds
reports to the exact active `(resource_plane_id, microvm_id,
allocation_generation)` tuple. Advancing the Workspace allocation retires
older Service runtime generations in the same transaction after the durable
Workspace runtime has accepted the replacement allocation; successful
Workspace deletion retires all remaining generations. Service deletion is not
confirmed while any prior generation remains unretired. The Resource Plane
also verifies that its journal still names the same running MicroVM and
generation after Guest reconciliation, and retries if allocation changed.
The signed report sink inspects the Control Plane's `accepted` response and
returns rejected reports to SQS instead of acknowledging them.

The Resource Plane SQS handler records failed records with a fixed
`command_delivery` failure class and the SQS message ID, then returns that
record for retry. It never logs the command body or raw provider/Guest error.
The consumer returns a fixed retry error for raw Guest and report-sink failures.
Local tests inject sensitive sentinel errors and verify neither logs nor the
consumer's returned error expose them.
