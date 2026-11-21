# ADR 0009: Workspace Lifecycle on Lambda MicroVMs

- Status: Accepted (API/SDK contract; AWS behavior PoC remains pending)
- Date: 2026-10-11
- Scope: Workspace runtime lifecycle in the Resource Plane. Workload Job runtime remains covered by [ADR 0008](0008-non-ec2-workload-runtime.md).

## Context

Hako's Workspace is a durable logical resource, while Lambda MicroVMs are
replaceable compute allocations. The fake Resource Controller already accepts
ensure-running, suspend, resume, and delete commands, but there is no production
Workspace runtime. The Workload Job adapter in Tasks 63–65 does not implement
Workspace persistence or lifecycle.

## Decision

Use the Lambda MicroVMs dedicated API and official Go SDK as the Workspace
compute provider. EC2 and Fargate Workload execution remain out of scope.
Separate the durable Workspace from its current provider allocation. A
Resource Plane-local durable journal fences each allocation by immutable
Tenant/Workspace/Resource Plane identity, Workspace revision, and operation
identity. Persist a deletion tombstone before terminating a MicroVM.

The runtime maps `ensure_running` to `RunMicrovm`, `suspend` to
`SuspendMicrovm`, `resume` to `ResumeMicrovm`, and `delete` to
`TerminateMicrovm`, then observes state through `GetMicrovm`. An API response
that begins a transition is not operation success. Duplicate launch attempts
reuse the exact immutable request and `ClientToken`; stale revisions are
rejected before provider effects.

Lambda MicroVM's documented maximum duration is 8 hours. Suspend preserves
memory and local disk only while that same allocation remains resumable; it
does not make the logical Workspace indefinitely running. Termination is
terminal. Hako will not automatically replace an expired/lost allocation until
Task 44 provides durable Workspace storage and Task 45 defines safe agent and
service recovery. Until then, missing allocation recovery is surfaced rather
than silently creating a fresh MicroVM.

The current documented MicroVM list/tag interfaces do not provide a verified
tag-based discovery path: `ListMicrovms` returns only VM/image/state metadata,
and the documented tag ARN resource types omit MicroVM. The RP journal is the
normal recovery authority; without a trustworthy journal/provider ID, stop for
operator reconciliation rather than adopting an unverified VM. Tag resources
only where their AWS APIs support Hako's ownership tags.

## Consequences

- Tasks 80–83 implement the contract in order: this ADR, durable journal/fence,
  provider adapter, then recovery and local fake-provider E2E.
- Task 82 must not claim configurable CPU/memory classes unless the provider
  API and quota contract prove them. Unsupported classes fail closed.
- EFS-backed source persistence and agent/service restart are prerequisites for
  automatic replacement; process memory cannot survive VM termination.
- AWS `ClientToken` retention, lifecycle races, tag compatibility, image/runtime
  behavior, network isolation, quota, and cost need an approved real AWS PoC.
- This ADR causes no AWS apply or external configuration change.

## References

- [Workspace Lambda MicroVM contract](../workspace-lambda-microvm.md)
- [Lambda MicroVM lifecycle](https://docs.aws.amazon.com/lambda/latest/dg/microvms-how-it-works.html)
- [RunMicrovm](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_RunMicrovm.html)
- [ListMicrovms](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_ListMicrovms.html)
- [TagResource](https://docs.aws.amazon.com/lambda/latest/microvm-api/API_TagResource.html)
