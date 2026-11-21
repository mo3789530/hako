# Resource Plane command/result protocol v1/v2

This document defines the Control Plane ↔ Resource Plane SQS trust boundary
and the wire compatibility rules for command and result JSON envelopes.

## Transport authentication and authorization

For AWS-to-AWS delivery, Hako relies on the authenticated AWS API request and
least-privilege IAM rather than placing a shared HMAC key in every message:

- The Dispatcher execution role receives `sqs:SendMessage` on registered
  command queues only. Each Resource Plane command queue resource policy names
  that exact role ARN as its only Control Plane sender.
- The Result Consumer execution role receives receive/delete/change-visibility
  permissions on registered result queues only. Each Resource Plane result
  queue resource policy names that exact role ARN as its only Control Plane
  consumer.
- The Resource Controller role receives receive/delete/change-visibility on
  its command queue and `sqs:SendMessage` on its result queue. It has no
  Control Plane database access and no general cross-account role assumption.
- SQS API calls use AWS's authenticated request signing and TLS. The queues use
  SQS-managed encryption at rest. No AWS access key, database credential,
  shared signing secret, or bearer token is included in a command or result.

The queue resource policy is the sender/consumer authorization boundary. A
message body is not a cryptographic proof of its producer. Operators must
preserve the exact IAM role ARNs and review CloudTrail/SQS access logs during
incident response. If a future transport cannot enforce principal-level
authorization (for example, a non-AWS BYOC relay), define a separately
versioned signature scheme with key rotation before enabling that transport;
do not reuse an implicit shared secret.

## Command envelope v1

```json
{
  "schema_version": 1,
  "operation_id": "op_123",
  "tenant_id": "tenant_123",
  "workspace_id": "ws_123",
  "resource_plane_id": "rp_tokyo_01",
  "type": "ensure_running",
  "created_at": "2026-09-25T00:00:00Z"
}
```

New writers use schema version 2, adding a positive Workspace generation:

```json
{
  "schema_version": 2,
  "workspace_revision": 4,
  "operation_id": "op_124",
  "tenant_id": "tenant_123",
  "workspace_id": "ws_123",
  "resource_plane_id": "rp_tokyo_01",
  "type": "resume",
  "created_at": "2026-09-25T00:00:00Z"
}
```

Supported operation types are `ensure_running`, `resume`, `suspend`, and
`delete`. The controller rejects unsupported versions/types, unknown JSON
fields, incomplete envelopes, or commands addressed to a different Resource
Plane. The Resource Plane ID is checked against local controller config; the
Control Plane result store also compares every identity field with the
Operation it previously issued.

## Result envelope v1

```json
{
  "schema_version": 1,
  "operation_id": "op_123",
  "tenant_id": "tenant_123",
  "workspace_id": "ws_123",
  "resource_plane_id": "rp_tokyo_01",
  "type": "ensure_running",
  "status": "succeeded",
  "observed_state": "running",
  "completed_at": "2026-09-25T00:00:01Z"
}
```

Schema version 2 results echo the command's `workspace_revision`. The Control
Plane initializes the revision on Workspace creation and increments it for
each explicit Desired State action or Reconciler claim.

Terminal status is `succeeded` or `failed`; a failed result includes a stable
`error_code` and `observed_state: failed`. Successful results must match the
operation state (`ensure_running`/`resume` → `running`, `suspend` → `suspended`,
`delete` → `deleted`). Results are applied only when Operation ID, Tenant ID,
Workspace ID, Resource Plane ID, operation type, and current durable Operation
state match. Version-2 results must also match the current Workspace revision.
Duplicate, terminal, or stale-generation results do not overwrite a newer state.
Malformed/untrusted result bodies are not applied or acknowledged as valid.

## Versioning and replay

Both envelopes are strict JSON: unknown fields are rejected. Version 1 remains
readable for queued-message compatibility and has no generation fence. Version
2 requires a positive `workspace_revision`. Deploy readers that accept both
versions before enabling version-2 writers; keep version-1 readers until old
messages drain or expire. A version number must never be reinterpreted in
place.

SQS delivery is at-least-once. `operation_id` is the retry idempotency key;
version 2's `workspace_revision` is the monotonic per-Workspace fencing token.
The Control Plane rejects a version-2 result whose revision is no longer
current. Each real Resource Plane Runtime must durably compare-and-set its
highest accepted revision (including delete tombstones) before any side effect;
the process-local Fake Runtime demonstrates this contract but does not survive
restart. Provider-backed durable fencing is not implemented yet.
`created_at` is also used as a bounded queue-age cutoff by the
Resource Controller (`HAKO_COMMAND_MAX_AGE`, default 30 minutes); commands
older than the cutoff fail with `operation_expired` without entering the
Runtime. A timestamp is not a signature or producer-authenticity proof.
Generation fencing rejects superseded commands when the runtime enforces the
durable watermark; it does not forcibly cancel work already executing. Runtime
timeouts and compensation/cleanup are still required for partial side effects.

## Implementation references

- [`resourcecontroller/controller.go`](../internal/resourcecontroller/controller.go)
  validates commands and emits result envelopes.
- [`operationresults/consumer.go`](../internal/operationresults/consumer.go)
  strictly decodes and applies results.
- [`resource-plane-queues-and-dlq.md`](resource-plane-queues-and-dlq.md)
  documents queue policies, IAM, redrive, and alarms.
- [`resource-plane-registration.md`](resource-plane-registration.md)
  defines the versioned Resource Plane manifest consumed by Dispatcher and
  Result Consumer.
