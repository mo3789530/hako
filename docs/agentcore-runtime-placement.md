# AgentCore Runtime Placement Decision

Status: Initial topology selected for implementation; provider behavior and security remain deployment gates.
Date: 2026-11-21

## Decision

Use one AgentCore Runtime per Hako deployment and AWS Region, with one opaque
AgentCore session for each immutable Hako Agent Run attempt and generation.
The Resource Plane owns Runtime configuration and invocation. The Control Plane
continues to own Tenant authorization, Run state, attempt identity, proposal
verification, and publication eligibility.

The durable Hako journal maps the full Tenant/Run/attempt/generation/Resource
Plane command to one session ID before invocation. The backend must authorize
that mapping on every dispatch and result. Callers cannot select a session ID,
Runtime ARN, Region, endpoint, or execution role from repository content or
queue payloads. An unavailable or disabled AgentCore backend fails the Run
without routing it to a Lambda MicroVM.

### Topology comparison

| Topology | Advantages | Cost and isolation tradeoff | Initial decision |
| --- | --- | --- | --- |
| Runtime per repository | Small repository blast radius | Resource count and provisioning scale with repositories; still requires attempt authorization and cleanup | Not selected |
| Runtime per Tenant | Tenant-specific runtime configuration and easier coarse billing | Resource count scales with tenants; execution role/session rules still need enforcement | Not selected for the initial deployment |
| Shared Runtime per deployment and Region | One image/configuration lifecycle and bounded Runtime resource count; separate session microVM per attempt | Shared execution role and capacity/quota; must not expose Tenant credentials or cross-attempt data | Selected, with Hako journal and broker APIs as the authorization boundary |

The Resource Plane owns Runtime provisioning, versioning, and deletion. A
Tenant does not own or directly select a Runtime. The attempt journal owns the
session mapping and cleanup fence; Control Plane owns Tenant/Run authorization
and result eligibility. Removing a Tenant does not delete a shared Runtime.

## Isolation and credentials

AgentCore Runtime's microVM compute type gives each session a separate
execution environment. This supports a shared Runtime resource while keeping
concurrent Run attempts in separate microVMs. It does not make the execution
role safe for arbitrary repository code. Code inside a session can retrieve
the session's execution-role credentials through the metadata endpoint.
Therefore the initial Runtime must not receive GitHub write credentials,
AWS source-bucket credentials, or broad Tenant data access. Deliver source and
collect proposals through attempt-bound Hako broker APIs. Limit model access
to the configured model and restrict network egress before enabling repository
code execution.

Use platform version V2 when session-context IAM conditions are needed. Those
conditions can scope supported AWS resource access using the session ID and
Runtime ARN, but they do not replace Hako authorization or the requirement to
minimize the Runtime role. Avoid direct S3 access in the first implementation
until the exact resource policy and session-key behavior have been verified.

## Lifecycle and storage

Keep managed session storage disabled initially. A stopped session can be
invoked again with the same ID, and configured session storage can preserve
files across stop/resume. Hako instead treats each attempt as immutable and
terminal after result and cleanup confirmation. Persist source and accepted
proposal artifacts in Hako's existing stores, not in an AgentCore session.

The Python harness workspace is a bounded in-memory snapshot for exactly one
`execute` call. It is populated only from the digest-verified immutable source
archive, has no host path or shared volume, and is discarded with the process.
Source artifacts remain owned by Hako's attempt storage; proposals are returned
through the attempt-bound result handoff and independently verified. A second
attempt gets a separately constructed `Workspace`, even for the same repository
and commit. Source or proposal size-limit violations fail closed before
publication. AgentCore session storage stays disabled; process restart must
reload the immutable source from Hako rather than resume untrusted workspace
files.

## Runtime HTTP contract

The initial image contract follows AgentCore's HTTP Runtime protocol: listen on
port 8080, answer `GET /ping`, and accept invocations on `POST /invocations`.
The service forwards invocation payloads without Hako schema validation. The
local Python entrypoint now implements `GET /ping` and a strict `POST
/invocations` contract. It accepts exactly `schema_version: 1`, an `attempt`
object (`tenant_id`, `run_id`, `attempt_id`, `resource_plane_id`, `generation`,
`repository_id`, `commit_sha`, `source_sha256`), a non-empty UTF-8 `task`, and
`source_archive_base64`. Duplicate JSON keys, unknown fields, malformed pins,
invalid base64, and oversized bodies are rejected. The source is capped at 40
MiB (56 MiB encoded request ceiling); the proposal response is capped at 12
MiB. The response identifies `proposal_ready` or `no_proposal` and carries the
same attempt pins plus the proposal ZIP and digest when present. It never
reports verifier success. The Hako verifier and durable journal remain
authoritative.

Resource Plane request construction uses
`workloadagentcore.EncodeInvocationRequest`. It accepts the trusted journal
attempt identity plus Repository ID, base commit, task and materialized
archive; recomputes SHA-256 over the archive bytes; checks the commit/source
pins and 40 MiB/64 KiB bounds; then emits the exact v1 JSON and base64 shape
above. A Go-to-Python integration test parses this body with the actual local
Runtime parser. This proves serialization only; it does not authenticate the
caller pins or connect a dispatcher to AWS.

`workloadagentcore.InvokeOnce` validates that the request matches the journal's
current attempt, requires a live journal-backed `AttemptLease`, and verifies
that the Runtime/session come from the frozen specification. The lease has
owner and expiry fields in the same CAS journal, so duplicate invocation,
cancellation, and cleanup workers can share one per-attempt serialization
boundary. Lease acquisition, renewal and release are conditional journal CAS
operations; stale owners cannot renew or release a successor lease. The caller
must renew before expiry and hold the lease through result checkpoint and
cleanup. `InvokeOnce` then compare-and-swaps `Launching` to `Running` before
calling an injected `RuntimeInvoker`. It decodes the response itself and returns only an
attempt-bound `InvocationDraft`, whose proposal has passed digest and bounded
ZIP input checks but remains unverified. A provider error, empty/oversized
response, or invalid response is returned as one fixed ambiguous-outcome error;
the journal stays `Running` and the invocation cannot be claimed a second time.
The dispatcher must hold its exclusive attempt lease and recheck cancellation
across this call. Fake-provider tests cover the pinned target, pre-call identity
rejection, malformed response fencing, and no replay after a lost response.
This interface does not stop the session, confirm cleanup, persist an artifact,
or publish a result.

`DecodeInvocationResponse` validates the response schema, rejects duplicate
and unknown JSON fields, rebinds every returned pin to the original request,
and verifies the decoded proposal bytes against the returned SHA-256. It then
runs the existing bounded proposal ZIP inspector to reject malformed archives,
protected paths, and credential markers before the bytes can reach artifact
storage. This is input hygiene only: the exact proposal still must be applied
to its pinned base and independently verified. It distinguishes `no_proposal`
from `proposal_ready`; neither outcome means verified. Tests cover malformed
base64/ZIP, foreign attempts, duplicate fields, protected paths, credential
markers, and modified proposal bytes.

`workloadcontroller.AgentCoreWorkloadResult` maps a decoded draft only when
Tenant/Run/attempt/generation/Resource Plane, Repository, base commit, and
source digest match the trusted Workload specification. Proposal bytes are
re-inspected and stored through the existing attempt-scoped Agent change-bundle
writer; the result is then handled by the shared Workload Controller, whose
journal and cleanup fence delay Control Plane publication until physical
session cleanup is confirmed. The local `AgentCoreWorkloadWorker` now composes invocation and the result bridge
under a journal-backed attempt lease. It persists the terminal result, confirms
cleanup before publication, and records `ResultPublished` so duplicate delivery
is a no-op. It fails closed on a recovered ambiguous `Running` invocation and
retries cleanup without invoking again. Fake tests cover success ordering,
duplicate delivery, cleanup retry, and lost invocation response. The Worker now has a concrete local input resolver. It reloads the exact frozen
Workload specification, retrieves Issue task text through the trusted
`AgentTaskProvider`, and reads the attempt-scoped archive through the existing
source archive reader. It validates the task context and immutable repository,
installation, branch, commit, and source digest pins before constructing the
request. The Worker has a bounded 20–30 minute attempt lease. It renews before the
synchronous Runtime invocation and at terminal-result, cleanup, and publication
phase boundaries. The 20-minute minimum covers the documented 15-minute
synchronous invocation ceiling plus a five-minute margin. It does not run a
background heartbeat during source resolution, artifact persistence, cleanup,
or publication; if one of those phases outlives the lease, the next renewal/CAS
fails closed. A background renewal loop and cancellation-safe shutdown remain
open. The SQS handler is not wired to a durable dispatcher/publisher or Lambda
binary, and production invoker, source and artifact clients are not composed.
This is not AWS execution or artifact-policy evidence.

The HTTP application does not authenticate Tenant or attempt identity. The
AgentCore IAM boundary authenticates the caller; only the trusted Resource
Plane dispatcher may invoke it, and that dispatcher must construct pins and
source data from the durable current attempt. IAM authorization to invoke a
Runtime does not make caller-supplied attempt fields trustworthy. Hako must
revalidate every returned identity and digest before accepting the artifact.
The handler suppresses request logging, returns generic errors, and marks
responses `no-store`; these local properties do not prove deployed IAM,
network, or log configuration.

The entrypoint reads the exact model and Region from deployment allowlists,
creates a regional Bedrock Runtime client, and limits connect/read timeouts.
The workflow checks its attempt deadline before and after every Converse call
and after proposal creation. A provider request already in progress can run up
to the configured 60-second read timeout beyond the local deadline; the
synchronous AgentCore API's 15-minute ceiling remains the outer hard limit.
The deadline is not a provider-side cancellation guarantee. Runtime packaging,
authenticated Resource Plane/SQS dispatch, independent verification, and
AWS-backed cleanup remain unimplemented. The local input resolver uses the
existing broker-provider and archive-reader interfaces but does not configure
those production clients. The local fakeable Worker provides journal fencing and cleanup
ordering only; it does not establish provider behavior.

The local `AWSRuntimeProvider` implements the invoker and stopper interfaces
with the official AWS SDK for Go AgentCore client. It sends the frozen Runtime
ARN, qualifier and session only, sets `RetryMaxAttempts=1` on each operation,
and caps/closes the streaming response at 12 MiB. Disabling SDK retries is
required because `InvokeAgentRuntime` has no client idempotency token; the
durable Worker owns ambiguity handling and must never replay a claimed invoke.
Session cleanup is confirmed only for a successful response whose optional
session ID matches the requested session, and retries reuse Hako's stable stop
token. Fake SDK tests cover request construction and failure bounds. This
adapter is locally testable, but no AWS request, deployed IAM contract or
provider cleanup behavior has been exercised.

`AgentCoreLambdaHandler` is a local AWS Lambda event adapter around the Worker.
It accepts at most ten SQS records, parses only the immutable six-field attempt
identity, and marks only failed message IDs for redelivery. Duplicate, unknown,
missing, oversized and trailing JSON input is rejected; logs expose only the
SQS message ID and a fixed failure class. This event adapter has fake tests but
has not been wired to the Resource Plane Lambda binary, Terraform queue mapping
or a durable AgentCore dispatcher.
Install the optional Python dependency with `pip install '.[runtime]'` when
building the Runtime image.

The synchronous invoke API has a 100 MB payload ceiling and a 15-minute request
timeout. Hako's source archive limit is 40 MiB; if transported inline in JSON,
base64 expansion and the other request fields must still remain under the API
limit. The Python policy timeout stays below the service maximum. The separate
asynchronous Runtime job interface has a different lifecycle and is not part
of the first implementation. These are protocol limits, not a tested Hako
runtime deployment.

Configure bounded idle and maximum lifetimes. The synchronous
`InvokeAgentRuntime` request limit is 15 minutes; the separate asynchronous
Runtime job feature allows longer work (up to eight hours on microVM compute)
and requires its own dispatch/result contract. The current Hako design uses
the synchronous invocation path, so its configured attempt timeout must stay
at or below 15 minutes. On completion, cancellation, timeout, or failure, stop
the session and record provider-confirmed cleanup before publishing a terminal
result. A stop timeout or ambiguous response remains cleanup-pending and is
reconciled. Never re-invoke a terminal attempt, even though AgentCore can
provision a new microVM for the same session ID.

### Hako attempt routing (local implementation)

The durable Workload scheduler now maps `job` attempts to `lambda_microvm` and
`agent` attempts to `agentcore`. The selected provider is included in the
immutable attempt Outbox command, including cancellation. The Control Plane
Dispatcher routes each command to the matching per-Resource-Plane queue. An
AgentCore queue must be explicitly configured as `agentcore_command_queue_url`;
if it is missing, the Outbox event remains retryable and is never sent to the
Lambda Workload queue. The Lambda Workload decoder rejects AgentCore commands,
and the bounded AgentCore SQS handler rejects Lambda-provider commands. Local
tests cover Agent Issue Run scheduling, provider pinning, queue routing, missing
queue fail-closed behavior, and consumer cross-route rejection. The Resource
Plane manifest/config and dispatcher routing are implemented. Terraform now
provides a separate default-off AgentCore command queue and DLQ, allows only
the exact Control Plane Dispatcher role to send, exposes their outputs, and
keeps the queue visibility timeout above the synchronous invocation ceiling.
Mock plan tests verify the default-off state and exact sender/resource policy.
The default-off AgentCore Worker Lambda and event source mapping are now composed. Its runtime IAM is scoped to the configured queue, exact Broker Secrets, S3 prefixes, KMS keys and allowlisted Runtime ARNs; optional VPC configuration is explicit. Module/dev `tofu validate` and ARM64 ZIP build pass. Creating the queue alone does not enable the Worker, and deployment remains opt-in. Continuous lease heartbeat, independent verifier hookup, Runtime image build, and deployed acceptance remain open.

### API behavior checked on 2026-11-22

AWS documents `InvokeAgentRuntime` as a streaming operation keyed by Runtime
ARN, optional qualifier, and `runtimeSessionId`. AgentCore validates the
session ID format but does not authorize that ID against the caller identity;
the Resource Plane journal mapping remains the tenant/attempt authorization
boundary. A repeat invocation with the same ID can continue that session. The
`InvokeAgentRuntime` request has no client-token idempotency field, so a timeout
or lost response cannot be retried as if it were a new safe attempt. Hako's
durable invocation fence must reconcile the existing session and prevent
re-invocation after result completion.

Hako records a one-time invocation claim by compare-and-swap from `Launching`
to `Running` before making the request. If the caller crashes or loses the
response, a second worker cannot claim the same attempt and invoke it again;
recovery must stop/reconcile that session and fail closed if completion cannot
be established.

The journal freezes the selected Runtime ARN, qualifier, and session before
the claim in separate `runtime_specification_payload` and
`runtime_specification_hash` fields. The existing `specification_payload` and
`specification_hash` remain owned by the Workload Controller and continue to
freeze the trusted Workload/source/artifact specification. Recovery reads the
AgentCore fields with `PreparedInvocationFromJournal`, which
rejects unknown JSON fields, unsupported backends, malformed Runtime targets,
and a session that does not match both the journal handle and deterministic
attempt identity. Recovery must use this decoded target instead of resolving a
Runtime from queue data. The helper validates journal integrity and identity;
it does not implement provider reconciliation or authorize an invocation.

AWS documents `StopRuntimeSession` as terminating the active session and any
ongoing streaming response. A successful response is HTTP 200. HTTP 409 means
the session is provisioning or tearing down and should be retried with bounded
backoff. HTTP 404 is documented as “session not found or already terminated”
and cannot distinguish an absent session from confirmed termination. Treating
404 as cleanup evidence therefore requires an independently durable record
that the exact session was previously allocated and that no invocation can
still be in flight; without that evidence, keep cleanup pending. The request
supports a `clientToken`; `workloadagentcore.StopClientToken` derives one from
the immutable attempt identity so retries of the same stop use the same token.
The token does not make invocation idempotent or prove cleanup by itself. These
API semantics are documentation findings, not deployed acceptance evidence.

For local cleanup composition, `PreparedCleanupFromJournal` applies strict
frozen-target checks to terminal attempts with cleanup pending/failed.
`StopAndConfirmCleanup` uses the attempt-derived `StopClientToken`, keeps
ambiguous failures unconfirmed, and compare-and-swaps to `CleanupConfirmed`
only after the injected stopper reports termination. It must run under the
same exclusive attempt lease used by invocation and cancellation. Fake tests
cover retry with the same token, failed-stop recovery, and rejection of
nonterminal/foreign sessions; no AWS SDK adapter or deployed cleanup evidence
exists.

References: [Invoke an AgentCore Runtime agent](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-invoke-agent.html),
[Runtime security model and session routing](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-instances-security.html),
[Stop a running session](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-stop-session.html),
[InvokeAgentRuntime API](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_InvokeAgentRuntime.html),
[StopRuntimeSession API](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_StopRuntimeSession.html).

## Region, limits, and ownership

Region is an explicit Resource Plane deployment setting. The first staging
candidate is `ap-northeast-1` when the selected Runtime feature set and model
are available there. Confirm model availability, account quotas, role policy,
and network controls during the staging preflight. Do not silently fail over
to another Region.

Current published Runtime limits include 5,000 active session workloads in
`us-east-1` and `us-west-2`, and 2,500 in other Regions. New session creation
has a shared 25 requests-per-second adjustable quota. These are account and
Region limits, not Hako Tenant quotas. Hako must enforce lower per-Tenant
admission limits and bounded retries. Recheck AWS quotas before deployment.

The Resource Plane deployment owns Runtime creation, image versioning, and
teardown. The Workload journal owns attempt/session mapping and cleanup state.
The Control Plane owns authorization and final Run projection. Teardown must
stop outstanding sessions before deleting a Runtime or its data.

## Consequences

- A shared Runtime avoids one AWS control-plane resource per Tenant while each
  attempt still gets a separate microVM session.
- The Runtime execution role is shared by sessions. Its permissions and
  network egress require strict limits and deployed evidence.
- Session IDs are not authorization tokens or invocation idempotency keys.
  Hako's journal lease, terminal fence, and result binding remain mandatory.
- Runtime image updates and model changes are deployment-controlled. Every
  result records the Runtime version, image digest, model, and policy revision.
- If deployed tests cannot establish the required role and egress boundary,
  keep managed execution disabled and revise the topology before launch.

## References

- [AgentCore Runtime security best practices](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-security-best-practices.html)
- [AgentCore Runtime session lifecycle](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-sessions.html)
- [AgentCore Runtime lifecycle settings](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-lifecycle-settings.html)
- [AgentCore Runtime quotas](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/bedrock-agentcore-limits.html)
- [AgentCore HTTP protocol contract](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-http-protocol-contract.html)
- [AgentCore supported Regions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/agentcore-regions.html)
