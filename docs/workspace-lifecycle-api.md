# Workspace lifecycle API

Workspace lifecycle requests are asynchronous. The Control Plane records the requested Desired State together with an Operation, its first event, and a Transactional Outbox command in one database transaction. A successful response means the request was durably accepted; it does not mean the Resource Plane has already changed the MicroVM.

## Request

```http
POST /v1/tenants/{tenant_id}/workspaces/{workspace_id}/actions
Authorization: Bearer <Cognito access token>
Idempotency-Key: <stable request key>
Content-Type: application/json

{"action":"suspend"}
```

`action` is one of `suspend`, `resume`, or `delete`. A successful request returns HTTP `202 Accepted`, the Workspace view (including Desired and Observed State), and the pending Operation ID/type/state. Reuse the same `Idempotency-Key` when retrying the same logical action. Reusing a key for a different action or Workspace returns `409 idempotency_conflict`.

The caller must be a Tenant member and must be allowed to see the Workspace under the existing visibility policy: Members can act on their own Workspaces; Tenant Owners/Admins can act on all Workspaces in their Tenant. A missing or hidden Workspace returns the same `404 not_found` response.

## State and conflicts

- `suspend` requires Desired State `running` and changes it to `suspended`.
- `resume` requires Desired State `suspended` and changes it to `running`.
- `delete` changes a non-deleted Workspace to Desired State `deleted`.
- A Workspace with a pending/running Operation rejects another action with HTTP `409 operation_in_progress`.
- An action that is invalid for the current Desired/Observed State returns HTTP `409 workspace_state_conflict`.
- Unsupported actions or malformed requests return HTTP `400 invalid_request`.

Desired State changes at acceptance time; Observed State changes only when an executor reports progress through the Operation transition path. A delete request does not release the Tenant's Workspace quota. The quota slot is released only when Observed State becomes `deleted`.

## Current implementation boundary

The API, store transaction, Operation event/outbox creation, API client, and `hako suspend|resume|delete` CLI commands are implemented. The CLI generates an idempotency key and prints it so a request can be safely retried with `--idempotency-key <key>`.

The Outbox Dispatcher delivers committed commands to configured Resource Plane SQS queues, and the Reconciler can create corrective Operations when Desired and Observed State drift; see [Outbox Dispatcher](outbox-dispatcher.md) and [Workspace Reconciler](reconciler.md). A Fake Resource Controller and process-local Fake Runtime consume commands; the Control Plane [Operation Result Consumer](operation-results.md) persists results. The full API lifecycle is covered by the PostgreSQL integration suite. Actual MicroVM lifecycle remains unimplemented, so fake execution does not change AWS resources. See [Fake Resource Controller](fake-resource-controller.md).
