# Preview Runtime command routing contract

This document defines the Control Plane Outbox event envelope accepted by the
Resource Plane queue router. It does not claim that a command producer,
Resource Plane consumer, provider adapter, or result consumer is deployed.

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
producer, consumer, result acknowledgment, and runtime execution remain
separate implementation work; no AWS changes were made for this contract.

See [Preview lifecycle](preview-lifecycle.md) and [Resource Plane registration](resource-plane-registration.md).
