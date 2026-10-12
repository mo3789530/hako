# Workspace Lambda MicroVM Runtime Adapter

`internal/workspaceruntime` is the Workspace lifecycle adapter for the official
Lambda MicroVM API. It is separate from the Workload Job adapter because a
Workspace has a durable identity and can outlive multiple compute allocations.

Before any provider call, the adapter requires a schema-v2 command with a
positive Workspace revision and compares it against the conditional
Resource-Plane journal. Runtime configuration comes only from a trusted
`SpecResolver`; commands cannot provide image, role, network, run-hook, or AWS
credential values. Runtime classes are allowlisted in adapter configuration,
and image/role/network identifiers, connector count, and the eight-hour API
limit are validated before an allocation request.

The exact serialized `RunMicrovm` request (excluding the provider-generated
MicroVM ID) and its digest are pinned in the journal before launch. The stable
client token is derived from Tenant, Workspace, Resource Plane, Workspace
revision, Operation ID, and launch-spec digest. If the launch response is lost,
the controller replays that frozen request and token; a later configuration
change cannot create a second allocation with different parameters. Provider
IDs are persisted with compare-and-swap before subsequent lifecycle calls.
State reads verify the provider ID, image/version, execution role, and network
connectors against the frozen allocation specification.

Delete writes a tombstone before cleanup. If it races an in-flight launch, the
controller replays the exact launch request to recover its ID, then terminates
and polls the allocation. A tombstoned Workspace cannot be recreated. Suspend
and resume retries inspect provider state before issuing another lifecycle API
call; termination uses the provider's idempotent terminate operation and then
polls until the allocation is confirmed gone.

The current MicroVM API contract does not provide verified per-MicroVM
ownership tags. The adapter therefore does not claim to tag the MicroVM; the
Resource-Plane journal key and verified provider ID are its ownership record.
It never discovers or adopts an unjournaled MicroVM by listing provider
resources. The Resource Plane Lambda execution role supplies AWS credentials.

This package is not wired into the resource-controller Lambda binary. That
binary remains fake-only, and no production runtime flag enables this adapter.
Image/container/network isolation and real AWS lifecycle PoCs remain required
before deployment. Tests use an SDK stub and local conditional-store fake; they
do not make AWS calls.
