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

After a successful non-delete operation, the Resource Controller obtains the
provider ID from the durable journal and includes it in the result envelope.
The Control Plane applies the result and the `lambda_microvm` row in
`resource_bindings` in one transaction. A successful delete removes that
binding in the same transaction as the terminal result; failed or stale results
do not mutate bindings. This projection is a lookup/binding record, not a
provider discovery mechanism.

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

The Resource Controller Lambda now has a local `lambda-microvm` composition
using this adapter, the S3 journal, and a strict Resource Plane-owned static
profile. Resource Plane Terraform now defines a default-off ARM64 deployment
input for this composition. It requires dynamic Workspace EFS roles, limits
MicroVM operations to one image and journal writes to one S3 object prefix,
and is mutually exclusive with the fake controller on the shared command
queue. The dev root leaves the input `null`; no MicroVM/journal permissions or
resources have been applied. Image/container/network isolation and real AWS
lifecycle PoCs remain required before deployment. Tests use SDK stubs,
conditional-store fakes, and OpenTofu mock plans; they do not make AWS calls.
