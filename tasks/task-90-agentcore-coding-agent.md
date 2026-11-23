# Task 90: AgentCore Runtime Coding Agent

Status: In Progress. Local Python/Go contracts, AgentCore invocation and cleanup worker, attempt-pinned Broker/S3 resolver, AWS SDK Runtime adapter, SQS queue/DLQ, default-off ARM64 Lambda consumer, and background attempt-lease heartbeat are implemented. Terraform mock coverage and module/dev validation pass; `make build-agentcore-worker-lambda` builds the ZIP. AgentCore proposals use the existing Workload result contract: the Control Plane stores the attempt-scoped change bundle and enqueues the independent verifier under the existing factory opt-in. Existing result-consumer PostgreSQL E2E and AgentCore result-bridge tests cover both boundaries. Remaining work is the digest-pinned Runtime image and AWS isolation/deployment acceptance. Priority: P3. Depends on Tasks 38, 39, 53, 87 and 89;
production verification also depends on the verifier and Broker deployment gates.

The unresolved design decisions and actionable follow-ups are tracked in
Tasks [91](task-91-agent-runtime-and-workspace.md)–[96](task-96-agentcore-lifecycle-operations.md).
Task 90 remains the parent acceptance and integration milestone.

Local result bridge follow-up (2026-11-22): the Workload Controller can now map a strict attempt-bound AgentCore draft to its existing `agent_changes` artifact metadata/result contract. It checks Tenant/Run/attempt/generation/Resource Plane, Repository/base commit/source digest and deadline against the trusted Specification; the proposal archive is re-inspected before storage. The existing Controller remains responsible for durable result checkpoint, cleanup confirmation and Control Plane publication. `make test-workload-runtime` and tagged Workload integration tests pass. No AgentCore provider, cancellation lease or AWS call was connected.

Local Worker composition follow-up (2026-11-22): added `AgentCoreWorkloadWorker` as a fakeable end-to-end Resource Plane worker. It acquires a journal-backed attempt lease, refuses to replay an ambiguous `Running` invocation after restart, resolves trusted invocation input through an injected resolver, checkpoints terminal Workload results, confirms session cleanup, then publishes and records `ResultPublished`. Duplicate delivery after publication is idempotent; cleanup failure can retry with the same stop token without invoking again. Focused worker tests pass. This is not yet an SQS/Lambda composition: the worker does not automatically renew its bounded lease, and no concrete authenticated source resolver or AWS AgentCore adapter is wired.

## Goal

Route managed Hako coding Agent Runs to Amazon Bedrock AgentCore Runtime.
GitHub Actions runners and interactive VS Code Workspaces run on Lambda
MicroVM. A request pinned to a Tenant, Repository and immutable commit
produces a bounded `agent_changes` proposal and verification evidence. Hako's
existing verifier and publication flow remain authoritative for branch and PR
creation. Keep this backend explicitly opt-in and default-off.

Reference: [AWS remote coding agent sample](https://github.com/aws-samples/sample-remote-coding-agent)
and [AgentCore Runtime documentation](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/agents-tools-runtime.html).
Use the sample's checkout, plan, code, verify and review sequence as guidance;
do not copy its independent control plane or direct PR publication path.

## Initial lifecycle contract

- Use microVM-backed Runtime sessions, one opaque deterministic session ID per
  immutable Hako attempt and generation. `workloadagentcore.SessionID` binds the
  entire trusted journal command; the journal and runtime configuration must
  pin the Runtime ARN and qualifier as well. Resolve these from trusted Resource
  Plane configuration, never from repository input or an untrusted queue body.
- Persist the session mapping and immutable invocation specification with a CAS
  fence before calling AWS. Dispatch, recovery and cancellation must all acquire
  the current attempt's exclusive lease and recheck its cancellation and terminal
  state immediately before each provider action. A session ID is not an
  idempotency guarantee: an ambiguous invocation must not be blindly replayed.
  Define durable agent-side invocation deduplication and recovery evidence before
  implementing invocation; if the outcome cannot be established, fail closed.
- Invocation completion may leave the session idle. Stop the session and record
  trusted cleanup evidence before result publication. AgentCore allows a stopped
  session to be started again by invocation, so prevent later invocation using
  the durable terminal fence and account for in-flight requests before cleanup
  confirmation. Investigate ambiguous stop/404 semantics against the deployed
  API before treating them as proof. Persistent session storage is disabled for
  the initial backend unless its deletion can also be established.
- A successful provider response is untrusted Agent output, not authorization to
  publish. Revalidate its attempt identity and proposal digest, then use the
  existing independent verifier and Task 89 Broker gates. Cancellation wins over
  late results; never publish a result before cleanup is confirmed.
- Do not enable this path until the role and egress boundary is demonstrably safe
  for repository code. AgentCore session isolation alone does not restrict what
  code in a session can do with its execution role.

The session ID and CAS-based journal preparation primitives have focused local
tests. Preparation freezes the Runtime ARN, qualifier and session ID before
invocation and rejects cancelled, stale or already prepared attempts. These
primitives are not wired to the scheduler or AWS and do not themselves provide
a lease, invocation deduplication, cleanup or isolation.

HakoCodingAgent is implemented in Python under `coding-agent/`. Its initial
offline workflow defines bounded plan/code/verify/review iterations, rejects
malformed evidence and does not mark unmeasured or changed trees verified.
The deterministic bounded proposal ZIP encoder accepts captured file bytes;
an integration test checks its output with the Go proposal inspector. It does
not support deletions or mode changes. An in-memory bounded workspace now tracks
changes from a caller-supplied snapshot, computes a local tree digest, and is
composed with injected model/verifier adapters and the workflow. Tests cover
rework, verifier mutation, review edits and proposal generation. The workspace
does not sandbox Python or establish verifier evidence. A digest-pinned source
tar loader validates and loads supported files without host extraction; a
Go/Python integration test exercises Go `CreateArchive` output. Protected paths,
executables and binary files are retained in the in-memory snapshot. Protected
paths are hidden from tools, binary files cannot be read by text tools, and
executables cannot be modified. Links, special files and oversized files remain
unsupported. Local tree digests include path, bytes and executable status.
A Bedrock Converse adapter now implements bounded model/tool turns with
read-only plan/review and proposal-validated writes for coding. Tests use a
synthetic client; no real Bedrock invocation or IAM/egress validation occurred.
Visible source file names and manifests now provide advisory model hints for
Python, Java, Go, Rust, TypeScript and Terraform. A local fake-model test
generates a proposal for each; this does not execute a language toolchain,
change the protected `infra`/`terraform` paths, or enable Terraform operations.
An offline execution function composes source digest validation, the model,
workspace and injected verifier for a pinned attempt; only locally verified
runs return proposal bytes. Its identity is caller-supplied binding metadata,
not authentication or independent verification. Tool rejection is returned as
a sanitized error so the model may recover within the existing call budgets.
Attempt-authorized source delivery, broader source support, a verifier adapter,
trusted tree measurement, proposal delivery and independent verification
integration remain open. Its local result is not publication evidence. The
Python package now also has a stdlib HTTP entrypoint with `/ping` and a strict
versioned `/invocations` request, regional Bedrock client creation, generic
error responses and fake-backed tests. The handler validates shape and size,
but does not authenticate caller-supplied attempt pins; only the trusted
Resource Plane may invoke it, and Hako must independently revalidate results.
Deadline checks stop additional model calls before/after each Converse call,
while an in-flight request can exceed the local deadline by up to the configured
read timeout (60 seconds). No Runtime image, deployed IAM boundary, provider
cleanup or end-to-end Hako handoff exists yet.

## Work

1. Specify the backend contract and lifecycle: immutable Tenant, Run, attempt,
   Repository and base SHA pins; runtime/session identity; admission, timeout,
   cancellation, retry, duplicate delivery, recovery and terminal result.
   Decide and document Runtime tenancy and session-to-attempt mapping after
   checking actual AgentCore isolation, lifecycle, region and quota limits.
   Preserve the existing MicroVM backend and do not route Job or Workspace
   workloads to AgentCore. Record how provider termination and cleanup are
   proven before adapting Task 89's cleanup gate; invocation completion alone
   must not be treated as cleanup confirmation.
2. Implement a default-off Resource Plane adapter and durable fenced invocation
   journal. Bind every callback/result to the current Run and attempt; reject
   stale or cross-Tenant results. Define how cancellation and ambiguous AWS
   responses are reconciled without accepting duplicate or late results. Use
   fake AgentCore and storage providers to test failure and recovery locally.
3. Build an ARM64 AgentCore Runtime image and entrypoint with a bounded coding
   loop. Obtain source for the pinned SHA through the existing trusted checkout
   path; treat task text and repository content as untrusted. Limit turns,
   execution time, output and proposal size. Report actual test execution and
   failure, never a synthetic success. The Agent must not obtain GitHub write
   credentials, publish a PR, merge, or deploy.
4. Specify and implement the exact source, model and artifact credential
   boundaries, including Runtime execution-role permissions, short-lived access,
   network egress and log redaction. A runtime role is an AWS credential: do not
   assume arbitrary repository code cannot access it. Demonstrate enforcement
   or move sensitive access behind a trusted broker before enabling execution.
   Restrict model and network access to the configured policy, and prove the
   effective boundary in the deployed environment.
5. Export the proposal in the existing attempt-bound `agent_changes` format,
   with bounded metadata and digest. Connect result ingestion to existing
   cleanup, verifier and Task 89 publication gates; verifier checks must bind
   the proposed tree and digest, not just the base commit. Failure, cancellation,
   timeout or invalid artifacts must not authorize publication.
6. Provide default-off Terraform for the Runtime, image, IAM, logging and
   necessary connectivity, with explicit region, model, image digest, limits,
   retention and cleanup ownership. Add deployment and teardown instructions,
   cost considerations and operational alarms. No secrets in Terraform state,
   task payloads, logs or artifacts. Verify actual provider/API capabilities
   and quotas before choosing concrete lifecycle and IAM mechanisms.
7. Add local composition tests covering admission through fake Runtime, result,
   verifier and publication eligibility, including duplicate delivery, lost
   response, stale attempt, cancellation, timeout, malformed artifact and
   cross-Tenant rejection. Add an opt-in AWS smoke procedure using a disposable
   trusted repository, including cleanup, isolation, network and IAM evidence.

## Acceptance criteria

- Default configuration does not provision or invoke AgentCore; enabling it is
  explicit and scoped to an authorized Tenant and Repository.
- A pinned Agent Run can produce one valid proposal through AgentCore and the
  existing verifier can independently validate the exact proposal. Publication
  continues through Task 89 and requires its existing policy and evidence.
- Local tests demonstrate retry fencing, cancellation and rejection of stale,
  foreign or malformed results. No simulated test is described as AWS evidence.
- AWS smoke evidence demonstrates actual session isolation, effective access
  and egress boundaries, cancellation/cleanup and artifact integrity. Until
  that evidence exists, production execution remains disabled.
- Documentation identifies deployed resources, ongoing costs, teardown order,
  supported region/model and remaining limits. AWS deployment and live GitHub
  writes are separate operational steps.

- Task 90 invocation-request follow-up (2026-11-22): added a Go v1 payload builder for the local AgentCore Runtime contract. It validates the immutable Hako command, repository/commit/source pins, UTF-8 task bound, archive size, and SHA-256 of the exact archive bytes before base64 encoding. Unit tests reject malformed pins and changed source; an integration test runs the body through Python `parse_invocation`. Focused Go and Go-to-Python tests pass. The Resource Plane dispatcher, authenticated source broker, provider invocation/cleanup, and deployed IAM remain open; no AWS call was made.

- Task 90 Runtime response follow-up (2026-11-22): added strict Go decoding for AgentCore responses. It rejects duplicate/unknown fields, foreign attempt pins, malformed status/base64 and digest mismatch; it returns explicitly unverified proposal bytes or a no-proposal outcome. `go test ./internal/workloadagentcore ./internal/workloadjournal -count=1` and the Go-to-Python invocation parser integration test pass. Runtime dispatch/source authentication, artifact storage, cleanup, independent verifier composition, and AWS adapter remain open.


Local brokered-input resolver follow-up (2026-11-22): added `BrokeredAgentCoreInvocationResolver`. It reloads the current journal by exact command, restores and validates the frozen Agent specification, resolves the Issue task through the existing trusted `AgentTaskProvider`, checks task context digest plus Repository/Installation/branch/commit pins, reads only the attempt-scoped source archive, and verifies its bounded size and SHA-256 before constructing the Go/Python invocation request. Fake tests reject foreign/changed tasks, wrong archive digest metadata, changed archive bytes, and foreign attempts. `make test-workload-runtime`, tagged Workload PostgreSQL tests, AgentCore/workloadjournal/controller race tests, and `git diff --check` pass. No AWS or GitHub service was contacted.

Runtime image follow-up (2026-11-22): added a non-root ARM64 Dockerfile using the SHA-256 pinned official Python 3.13 slim Bookworm multi-architecture manifest, plus a complete boto3 runtime wheel lock with per-artifact hashes. The root Makefile exposes `build-agentcore-coding-agent-image`, which uses Kiri and does not publish. AWS documentation confirms ARM64 and the default 8080 `/ping` and `/invocations` HTTP contract. The local Kiri build could not complete: Kiri is stopped, its Linux daemon requires root (`sudo kiri daemon`), passwordless sudo is unavailable, and the Kiri BuildKit reported Docker Hub DNS failure. No Podman fallback or AWS call was made. A successful ARM64 image build and inspection remain outstanding.


Independent verifier path review (2026-11-23): AgentCore draft results are converted to the ordinary `WorkloadResult` with an attempt-scoped `ChangeBundle`; `workloadjobs.ApplyWorkloadResult` already persists that artifact and enqueues the existing isolated verifier only for factory-enabled Tenant repositories. The AgentCore result bridge and PostgreSQL-backed verifier enqueue/binding tests pass separately. The standard result contract preserves the independent verifier boundary; no AgentCore-specific verifier implementation remains. Live verifier execution and rollout are deployment acceptance.
