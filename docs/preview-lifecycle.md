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
private table and are not included in the Tenant-facing Preview list. The
existing cleanup confirmation transitions all absent generations to `cleaned`
only after its executor confirms PR-level runtime deletion. These are durable
metadata transitions only; no Preview MicroVM or Gateway route is created yet.

Migration 000160 adds bounded provision claims to those generation rows: claim
ID, lease expiry, attempt count, fixed failure class, and retry deadline.
`ClaimRuntimeAllocations` claims only the current unexpired PR-head generation
and can reclaim an expired worker lease. `BindRuntimeAllocation` records
provider IDs only while the same generation, claim, attempt, and lease are
still current; a stale result is rejected and its caller must compensate any
provider resources it already created. Provision failures use fixed error
classes and bounded exponential retry. `internal/previewprovision.Worker` now
executes that claim/provision/bind/retry state machine through an injected
Resource Plane provisioner; if a result loses its generation fence, it invokes
the provisioner's idempotent compensation method. PostgreSQL lifecycle
integration drives a fake provisioner through failure/backoff, successful
binding, and a PR-head change during provisioning that rejects and compensates
the stale result. The focused target reports 72.4% statement coverage for the
provisioner package. A concrete Resource Plane command, provision-worker
deployment and DSQL role grant, Lambda MicroVM API call, and Gateway route
publisher are still not wired, so no Preview runtime is allocated.

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
confirmation is idempotent. Migrations 000145–000146 add expiring, attempt-fenced
cleanup claims so crashed workers can be recovered and stale workers cannot
acknowledge a newer claim. Migration 000147 adds lifecycle generations, which
also fence cleanup metadata against later PR head/state changes. The local
`internal/previewcleanup` worker now claims bounded batches, calls an injected
idempotent executor under a deadline capped by the claim lease, and only marks
the row cleaned after executor confirmation. Fixed provider error categories
schedule a retry while keeping the quota reservation. PostgreSQL integration
verifies failure/backoff, attempt and claim changes, confirmed cleanup, and
duplicate sweep behavior. The actual executor that resolves Preview-to-runtime
binding and deletes the MicroVM/Gateway route, plus its scheduled deployment,
is still required before Preview can be enabled for user traffic.

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

When runtime execution is added, it must bind the immutable PR head SHA to an
isolated Workspace, enforce Tenant authorization on every route, respect the
Tenant Preview count/lifetime quota, and make close/merge/expiry cleanup
idempotent. Preview data must not reuse a developer Workspace volume or expose
database/application ports directly. Production deployment remains subject to
the Task 43–46 runtime/Gateway gates.

See [Task 54](../tasks/task-54-preview-catalog-deploy.md).
