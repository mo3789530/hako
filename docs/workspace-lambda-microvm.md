# Workspace Runtime Contract: Lambda MicroVMs

Status: API/SDK contract documented locally; no real AWS Workspace launch or
lifecycle PoC has been run. This contract does not enable production execution.

## Resource model

`Workspace` is Hako's durable logical resource. A Lambda MicroVM is an
ephemeral allocation attached to that Workspace for one allocation revision.
There can be at most one current allocation for a Workspace. The provider ID
is not the Workspace ID and is never an authorization identifier.

The Resource Plane durable journal is the authority for fencing side effects
and recovering an allocation after controller restart. The Control Plane's
`resource_bindings` row is a projection updated through the authenticated
result path; the Resource Plane must not connect to Control Plane DSQL. A
journal entry fixes Tenant, Workspace, Resource Plane, Workspace revision,
operation ID, stable launch token, provider ID (when known), and an immutable
launch-parameter digest. A mismatch is a conflict, not an invitation to adopt
or overwrite another allocation.

## Lifecycle contract

The Resource Controller reports success only after `GetMicrovm` confirms the
target stable state. An API acknowledgment that merely starts a transition is
not observed-state completion.

| Hako operation | Lambda MicroVM API | Required completion condition |
| --- | --- | --- |
| `ensure_running` | `RunMicrovm` with a stable `ClientToken`, then `GetMicrovm` | `RUNNING`; reuse the journaled live allocation, never launch a second one for the same revision |
| `suspend` | `SuspendMicrovm`, then `GetMicrovm` | `SUSPENDED`; memory and local disk are preserved only for this allocation |
| `resume` | `ResumeMicrovm`, then `GetMicrovm` | `RUNNING`; only valid while the same allocation is `SUSPENDED` |
| `delete` | persist deletion tombstone, `TerminateMicrovm`, then `GetMicrovm` | `TERMINATED`; provider termination is idempotent, cleanup state remains retryable until observed |

Error handling contract: validation and access-denied errors are configuration
failures and must not be retried unchanged; quota errors are surfaced and
retained for operator action; throttling/server errors use bounded
`Retry-After`-aware retries. A conflict triggers `GetMicrovm` and reconciliation
of the journaled allocation, not a second launch. A not-found response for a
persisted allocation means the allocation is lost; it is not permission to
replace it before durable storage and agent recovery are enabled. Any timeout
after a side effect is ambiguous and must retain the checkpoint and exact
request identity.

Intermediate provider states map as follows: `PENDING`→`provisioning`,
`SUSPENDING`→`suspending`, and `TERMINATING`→`deleting`. The command result
protocol currently records terminal operation results, so the adapter must
poll with a bounded context rather than report an intermediate state as
success. If the operation deadline expires, preserve the journal checkpoint
and retry/reconcile; do not issue a new operation identity or launch token.

Revision fencing is checked and journaled before every provider side effect.
A delete tombstone is durable before termination. A later ensure/resume with
an older revision is rejected before AWS calls. For the same revision, duplicate
delivery must reuse the exact operation and launch request. Keep cleanup failures
in the journal for retry and operator visibility.

## Limits and recovery

- The documented `RunMicrovm` maximum duration is 28,800 seconds (8 hours).
  Treat it as an allocation lifetime ceiling; do not promise an indefinitely
  running Workspace or assume suspend pauses that ceiling. The behavior of the
  lifetime clock across suspension must be confirmed in the AWS PoC.
- `SUSPENDED` preserves memory and local disk for the same MicroVM. `TERMINATED`
  is terminal and cannot be resumed. Replacing a terminated allocation loses
  process/memory state and any unreplicated local disk state.
- Therefore automatic replacement is unsupported until Task 44's durable
  `/workspace` storage and Task 45's agent/service recovery contract are
  complete. Before then, a missing/expired allocation is surfaced as failed or
  requiring explicit recovery; it is not silently recreated.
- The official `RunMicrovm` request exposes no CPU/memory size fields. Hako
  `small`/`standard`/`large` classes must not imply configurable capacity until
  AWS capability and quota evidence exists; unsupported classes fail closed.
- The API allows at most 10 ingress and 10 egress connector identifiers per
  request. This is a request-shape limit, not proof that a connector enforces
  Hako's Tenant isolation or deny-by-default policy.
- MicroVM API exposes `RUNNING`, `SUSPENDING`, `SUSPENDED`, `TERMINATING`, and
  `TERMINATED`; transition conflict, throttling, quota, timeout, and not-found
  responses are retry/reconciliation inputs, not proof that a side effect did
  not occur.
- `RunMicrovm.ClientToken` is the provider idempotency mechanism. AWS documents
  its purpose but the retention window and concurrent launch/terminate race
  behavior remain unverified. Keep the immutable launch intent and token in the
  RP journal; ambiguous outcomes require same-request recovery and must not
  switch parameters or tokens. Long-delayed retries remain an AWS PoC gate.

## Tags and provider discovery

The documented `ListMicrovms` filters only by image identifier and image
version, returns no tags, and is paginated. The documented `TagResource` /
`ListTags` ARN pattern includes `microvm-image` but not a MicroVM resource ARN.
Consequently Hako cannot currently rely on tags to rediscover an allocation or
prove a MicroVM is safe to adopt. Do not scan and adopt/delete arbitrary VMs.
Use the RP journal and fixed provider ID for normal recovery; if that binding is
lost and identity cannot be proven, stop for operator reconciliation. Apply
`docs/aws-resource-tags.md` tags to AWS resources whose APIs actually support
them (for example EFS and connectors), and retain the same ownership metadata
in the RP journal for MicroVMs.

## Network, image, and security boundary

MicroVM images are built from a Dockerfile into a snapshot. The build snapshot
can include initialized processes, but unique per-Workspace state must be
initialized at runtime and secrets must not be baked into a shared image.
Ingress/egress connectors are explicit launch parameters. The service's
default Internet egress is not an isolation policy; production must use an
approved connector and separately verified VPC deny-by-default controls.
Never pass AWS credentials, GitHub App private keys, or secret values in the
launch command or `/run` payload. Runtime IAM stays in the Resource Plane.

An egress connector routes MicroVM outbound traffic into its configured VPC; it
is not itself a NAT gateway. VPC-local and VPC-endpoint traffic use VPC routes.
Internet-bound IPv4 traffic traverses a NAT gateway only when the connector's
subnet route table directs `0.0.0.0/0` to that NAT gateway. Without that route,
Internet access fails; without an egress connector, Lambda MicroVMs retain the
service's default public Internet egress. A NAT gateway does not enforce an
FQDN allowlist, so untrusted Workload egress additionally needs a separately
verified firewall or egress proxy policy. Hako has not deployed or tested the
connector, route table, NAT gateway, or filtering path.

The API/SDK exposes suspend, resume, terminate, tags, and list calls locally,
but actual isolation, endpoint/auth behavior, hook behavior, quota, costs,
tag compatibility, client-token retention, and state-race behavior still need
an approved AWS PoC. No AWS resource is created by this contract.

## References

- [Lambda MicroVM lifecycle](https://docs.aws.amazon.com/lambda/latest/dg/microvms-how-it-works.html)
- [RunMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_RunMicrovm.html)
- [SuspendMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_SuspendMicrovm.html)
- [ResumeMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_ResumeMicrovm.html)
- [TerminateMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_TerminateMicrovm.html)
- [ListMicrovms](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_ListMicrovms.html)
- [TagResource](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_TagResource.html)
- [ListTags](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_ListTags.html)
