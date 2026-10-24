# Pull Request Preview lifecycle

Repository Preview tracking is opt-in per Tenant GitHub Repository. Tenant
Owners/Admins configure it through:

```text
PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/preview
{"enabled": true}
```

The CLI equivalent is `hako github repository preview enable|disable --tenant
<tenant> --installation <id> --repository <id>`.

Tenant members can inspect the bounded lifecycle list for that registered
Repository:

```bash
hako github repository previews list --tenant <tenant> \
  --installation <installation-id> --repository <repository-id> --limit 50
```

The matching read-only endpoint is
`GET /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/previews?limit=50`.
It returns PR number, pinned head SHA/ref, a monotonic lifecycle `generation`,
desired/observed lifecycle state, expiry, cleanup retry metadata, and update
time. The limit defaults to 50 and is capped at 100. Tenant membership is
required; an Installation or Repository outside that Tenant is not disclosed.
`runtime_enabled` remains false because this listing reports lifecycle
metadata only.

When a runtime allocation for the current PR generation is marked `running`,
Tenant members can request a short-lived Gateway Session:

```text
POST /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/previews/{pull_request_number}/sessions
{"permissions":["vscode"]}
```

The response contains the logical runtime Workspace ID, opaque Session ID,
Gateway ticket, permitted channels, and expiry. It never returns the Lambda
MicroVM ID or Gateway provider route ID. The CLI equivalent is
`hako github repository previews session create`; its JSON includes the bearer
ticket, so treat the output as a secret. Revoke it with
`hako github repository previews session revoke --tenant ... --installation
... --repository ... --pull-request ... --session ...`.

For a running Preview, open its IDE through the loopback-only CLI tunnel:

```bash
hako github repository previews open --tenant <tenant> \
  --installation <installation-id> --repository <repository-id> \
  --pull-request <number> [--local-port 0]
```

The CLI creates a fresh Preview Session for each Gateway WebSocket connection,
uses the logical runtime Workspace ID returned by that API response for
routing, and revokes the Session when the connection closes. The local listener
binds only to `127.0.0.1`. Until a Resource Plane provisioner binds a running
Preview generation and publishes its Gateway route, the API rejects session
creation and `open` cannot connect; this command does not create runtime
resources.

Preview session records are separate from developer Workspace sessions because
Preview runtime identities are not rows in the public `workspaces` table.
Resource Plane active-session lookup checks both stores. For Preview tickets it
also verifies that the pinned generation remains the current present generation
and that its allocation and Preview remain unexpired; PR close, head update,
expiry, or explicit revoke therefore invalidates an existing ticket immediately.
The ticket expiry is capped at the Preview expiry. Session create/revoke events
are audited. These APIs are available only when GitHub Repository APIs and the
Workspace Session ticket issuer are configured, and return a conflict until a
provider-bound running allocation exists; they do not themselves allocate a
MicroVM or publish a route. The scheduled Preview expiry batch also deletes a
bounded number of expired Preview Session rows using the session-expiry index.

The endpoint writes an audit event. The webhook processor ignores deliveries
received before `preview_enabled_at`, so enabling Preview does not backfill old
PR events. Disabling Preview prevents new lifecycle updates; it does not delete
existing state.

For opted-in repositories, authenticated `pull_request` deliveries update
`repository_previews`, keyed by Tenant, Installation, Repository, and PR. The
inbox acknowledgement and lifecycle update share one database transaction.
Opened/reopened/synchronize events set desired state to `present` and state to
`awaiting_runtime`. Closed events set desired state to `absent` and state to
`cleanup_pending`. GitHub's PR `updated_at` timestamp fences delayed older
deliveries; the webhook receipt timestamp is used only when that field is
missing. Equal timestamps use Delivery ID as a deterministic tie-breaker.
Duplicate deliveries are suppressed by the durable webhook inbox.

Each Preview starts at generation 1. A newer accepted PR event increments the
generation once when it changes the effective head SHA or desired state; an
older event and a same-head synchronize do not. Closing, reopening, and expiry
therefore advance the lifecycle generation. Cleanup claims carry the generation
they observed, and retry/completion writes require that exact generation in
addition to the claim ID, attempt, and lease. This fences a delayed cleanup
worker if the Preview lifecycle has moved on since its claim.

Migration 000159 adds a private `repository_preview_runtime_allocations` row
for each admitted present PR generation. It pins the immutable head SHA/ref and
expiry separately for each generation. A head update or close moves every
older active generation to `cleanup_pending` in the same webhook transaction;
expiry does the same in its bounded sweep. Reopen creates a new generation
without overwriting old cleanup records. Provider IDs are reserved for this
private table and are not included in the Tenant-facing Preview list. Each
generation now has its own cleanup lease and acknowledgement; the older
PR-level cleanup path is restricted to Previews that have no runtime-allocation
rows. These are durable metadata transitions only; no Preview MicroVM or
Gateway route is created yet.

Migration 000160 adds bounded provision claims to those generation rows: claim
ID, lease expiry, attempt count, fixed failure class, and retry deadline.
`ClaimRuntimeAllocations` claims only the current unexpired PR-head generation
and can reclaim an expired worker lease. Each claim carries a deterministic
private runtime Workspace ID derived from Tenant, Installation, Repository, PR,
and generation, so retries of one generation converge and later PR heads get a
different runtime identity. `BindRuntimeAllocation` records
provider IDs only while the same generation, claim, attempt, and lease are
still current and the returned Workspace ID matches that deterministic ID; a
stale or mismatched result is rejected and its caller must compensate any
provider resources it already created. Provision failures use fixed error
classes and bounded exponential retry. `internal/previewprovision.Worker` now
executes that claim/provision/bind/retry state machine through an injected
Resource Plane provisioner. After every failed attempt, the worker invokes
compensation with the partial binding, if any, and the immutable generation
identity so the Resource Plane can discover unbound resources by Hako tags
before retry. A compensation failure leaves the claim for lease-based recovery
instead of pretending cleanup succeeded. If a successful result loses its
generation fence, it also invokes idempotent compensation. PostgreSQL lifecycle
integration drives a fake provisioner through partial MicroVM creation followed
by route-publish failure and cleanup, retry/backoff, successful binding, and a
PR-head change during provisioning that rejects and compensates the stale
result. It also verifies that a failed compensation does not schedule a
duplicate attempt while the lease is active, then recovers after lease expiry.

Migrations 000165–000169 add and constrain a target Resource Plane pin and
private capacity reservation to each runtime generation. The first provision
claim stores its selected Plane before provider I/O; retries cannot move a
generation to another Plane. Bound results must match this pin, and cleanup
jobs carry it together with the deterministic runtime Workspace ID even when
no provider IDs were persisted. A worker may use an explicit Plane for
controlled deployments or omit it and provide a Runtime Class to select from
active, healthy Planes. Both modes reserve configured Workspace/CPU/RAM
capacity on the first claim. Dynamic selection additionally applies Tenant
placement policy and requires the `microvm` capability. Capacity remains charged across provisioning
retries and is released only after cleanup confirmation. A conditional claim
race reverses its provisional capacity reservation in the same transaction.
The claim uses conditional writes and OCC retries rather than row-locking
syntax, keeping it compatible with Aurora DSQL. The Outbox Dispatcher now has
a dedicated Preview provision/cleanup queue route with strict generation,
claim, logical identity, and Plane validation; it never falls back to the
Workspace command queue. The producer, Resource Plane consumer/result path,
and queue infrastructure are still missing. PostgreSQL integration covers
Tenant region/Plane filters, full-capacity exclusion, pinned placement, and
capacity release after cleanup. Resource Plane command transport, worker
deployment and DSQL role grants, Lambda MicroVM API calls, Gateway route
publication, and tag-based provider executor are still missing, so no Preview
runtime is allocated for user traffic.

The accepted Preview Outbox envelope and current routing-only status are
documented in [Preview Runtime command routing contract](preview-runtime-command-contract.md).

Each present Preview records a fixed `expires_at` using the Tenant's configured
Preview lifetime (24 hours when no quota row exists). `synchronize` deliveries
update the pinned commit without extending that lifetime; a valid `reopened`
event starts a new lifetime. The bounded `previews.ExpireDue` store operation
transitions expired rows to `desired_state=absent,state=cleanup_pending` and is
idempotent. It never marks a Preview `cleaned`; a later runtime cleanup worker
must confirm physical deletion before doing so. Migrations 000142–000144 add
the expiry column and index, and backfill existing present records using their
Tenant quota or the 24-hour fallback.

## Runtime and cleanup status

This is lifecycle metadata only. No MicroVM is created and no Gateway route is
published: responses report `runtime_enabled: false`, and records remain in
`awaiting_runtime`, `quota_exceeded`, `cleanup_pending`, or `cleaned`. The existing Lambda MicroVM Workspace
runtime does not yet provision a PR-scoped Workspace binding or expiring
Tenant-scoped Gateway route. A default-off scheduled Lambda invokes the bounded
expiry sweep, but it is not enabled by default and only updates DSQL metadata.
The store records only fixed cleanup error categories and schedules bounded
exponential retries (5 seconds to 1 hour). `cleaned` is written only after the
runtime executor confirms MicroVM and Gateway route deletion, and duplicate
confirmation is idempotent. Migrations 000145–000147 add parent cleanup leases
and lifecycle generations. Migrations 000161–000162 add separate lease,
attempt, and retry state to each runtime generation. The local
`internal/previewcleanup.RuntimeWorker` claims bounded generation batches and
uses immutable provider bindings, or the deterministic private runtime
Workspace ID plus pinned Resource Plane ID when an interrupted provision
attempt has no binding, and marks a generation cleaned only after an injected
executor confirms deletion. It can
also retire generations that never had a provision attempt without calling a
provider. The legacy PR-wide worker
does not claim Previews that have generation rows, preventing it from
acknowledging those rows without per-generation confirmation. The quota remains
reserved until every generation is cleaned. PostgreSQL integration verifies
lease recovery, active-claim exclusion, retry/backoff, stale-claim rejection,
provider-bound cleanup, interrupted unbound-attempt tag-recovery identity, and
final quota release. A concrete Resource Plane cleanup command/executor that
implements tag discovery, and scheduled worker deployment are still required
before Preview can be enabled for user traffic.

Webhook admission reserves the Tenant Preview concurrency slot in the same
transaction as the PR lifecycle update, using the shared Tenant quota OCC row.
The slot covers both PR Previews and Workload Preview Runs; `cleanup_pending`
retains the slot until `cleaned` confirms physical deletion. When capacity is
exhausted, the PR is recorded as `desired_state=absent,state=quota_exceeded`
without creating a runtime cleanup job. A later `reopened` event retries
admission; closing a quota-rejected PR moves it directly to `cleaned`. Usage
and quota reconfiguration include active PR Preview and cleanup reservations.
`make test-preview-expiry-integration` exercises the list API's generation
field and Tenant scope, plus the store's claim, active-lease exclusion, fixed
retry backoff, crash recovery, stale-claim/generation rejection, confirmed
completion, and duplicate acknowledgement against an isolated local PostgreSQL
schema. It also exercises the cleanup worker against a fake executor. This
verifies the metadata fence and worker orchestration only; it does not simulate
provider deletion or prove runtime cleanup.

When runtime execution is enabled, it must bind the immutable PR head SHA to an
isolated Workspace, enforce Tenant authorization on every route, respect the
Tenant Preview count/lifetime quota, and make close/merge/expiry cleanup
idempotent. Preview data must not reuse a developer Workspace volume or expose
database/application ports directly. Production deployment remains subject to
the Task 43–46 runtime/Gateway gates.

See [Task 54](../tasks/task-54-preview-catalog-deploy.md).
