# Task 90: AgentCore Runtime Coding Agent

Status: In Progress (Python workflow core, session identity and journal preparation primitives, and initial lifecycle contract;
backend dispatch, AWS integration and execution are not implemented). Priority: P3. Depends on Tasks 38, 39, 53, 87 and 89;
production verification also depends on the verifier and Broker deployment gates.

The unresolved design decisions and actionable follow-ups are tracked in
Tasks [91](task-91-agent-runtime-and-workspace.md)–[96](task-96-agentcore-lifecycle-operations.md).
Task 90 remains the parent acceptance and integration milestone.

## Goal

Add Amazon Bedrock AgentCore Runtime as an execution backend for Hako coding
Agent Runs. A request pinned to a Tenant, Repository and immutable commit
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
trusted tree measurement, AgentCore entrypoint, proposal delivery and
independent verification integration remain open. Its local result is not
publication evidence.

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
