# Preview Runtime command routing contract

This document defines the Control Plane Outbox event envelope accepted by the
Resource Plane queue router. A local database-only producer now commits Preview
claims and provision/cleanup events atomically. PostgreSQL integration follows
the committed commands through the Dispatcher, fake Resource Plane controller,
and generation-fenced result projection. Provider adapters and AWS queue/Lambda
deployment are not wired.

## Producer transaction

`internal/previewdispatch.Worker` calls the Preview store claim operations and
inserts provision/cleanup events inside the same PostgreSQL/DSQL transaction.
Provision transactions also pin the generation to its Resource Plane and
reserve configured capacity. Cleanup transactions lease retired generations;
they enqueue provider cleanup only when a provider may own resources, and mark
never-attempted generations clean locally. If command encoding or Outbox
insertion fails, the transaction rolls back the claim, reservation, and local
cleanup transition together. A separate Outbox Dispatcher publishes after
commit. Re-running while a claim lease is active does not enqueue a second
command.

Event IDs and the claim ID are allocated before entering the OCC-retried
transaction callback, so transaction retries reuse stable identifiers without
performing external side effects.

## Resource Plane consumer and result

`internal/previewcontroller.LambdaHandler` validates the SQS message's
`HakoEventType` and `AggregateID` attributes against the strict body, then
rejects commands pinned to another Plane. Provisioning is bounded by the
earliest of the configured execution timeout, claim lease, and Preview expiry.
The injected Runtime contract requires idempotency by runtime Workspace and
generation. Partial provision failures are compensated before a failure result
is reported; if compensation or result delivery fails, the SQS record is
returned as a batch failure for retry. Provider error strings are not copied
into results or logs.

The Resource Plane result Queue receives a generation- and claim-fenced result
envelope with fixed status/error codes and optional provider binding. FIFO
queues use Workspace generation ordering and a deterministic deduplication key
for `(action, workspace, generation, claim, attempt)`. The Control Plane
`internal/previewresult.LambdaHandler` and
`store/previews.ApplyRuntimeResult` compare the result with the active
generation, claim, attempt, lease, derived runtime Workspace ID, and pinned
Plane before applying it. Stale or duplicate claims are ackable no-ops;
malformed or mismatched results remain retryable. PostgreSQL integration
composes producer, Dispatcher, fake Resource Plane controller, and result
projection. The Lambda MicroVM/Gateway provider and result queue deployment are
not wired.

## Outbox event

The Dispatcher accepts only these event identities:

```text
aggregate_type = repository_preview_runtime
aggregate_id   = prvws_<32 lowercase hex characters>
event_type     = repository_preview_runtime.provision
              | repository_preview_runtime.cleanup
```

It validates a strict version-1 JSON envelope before looking up the queue by
the pinned `resource_plane_id`:

```json
{
  "schema_version": 1,
  "action": "provision",
  "tenant_id": "tenant_123",
  "installation_id": 17,
  "repository_id": 23,
  "pull_request_number": 4,
  "generation": 2,
  "runtime_workspace_id": "prvws_0123456789abcdef0123456789abcdef",
  "resource_plane_id": "rp-tokyo-01",
  "claim_id": "preview-provision-1234",
  "claim_attempt": 1,
  "lease_expires_at": "2026-10-25T02:00:00Z",
  "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "head_ref": "feature/preview",
  "expires_at": "2026-10-26T02:00:00Z"
}
```

`cleanup` uses the same claim/generation/Plane identity but omits `head_sha`
and `head_ref`. The `action` must match `event_type`; `runtime_workspace_id`
must equal `aggregate_id`. Unknown fields, inconsistent identities, malformed
SHA, and unmapped Plane IDs fail closed. Missing Preview queue configuration
leaves the Outbox event retryable and never routes it to the Workspace command
queue.

The optional per-Plane manifest setting is
`preview_runtime_command_queue_url`. It must be a distinct SQS URL in the
registered Resource Plane account and region. Queue creation, exact IAM,
Resource Plane consumer, result acknowledgment, provider-backed MicroVM and
Gateway work, and scheduled deployment composition remain separate work; no
AWS changes were made for this contract.

See [Preview lifecycle](preview-lifecycle.md) and [Resource Plane registration](resource-plane-registration.md).
