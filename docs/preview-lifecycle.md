# Pull Request Preview lifecycle

Repository Preview tracking is opt-in per Tenant GitHub Repository. Tenant
Owners/Admins configure it through:

```text
PUT /v1/tenants/{tenant_id}/github/installations/{installation_id}/repositories/{repository_id}/preview
{"enabled": true}
```

The CLI equivalent is `hako github repository preview enable|disable --tenant
<tenant> --installation <id> --repository <id>`.

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
`awaiting_runtime` or `cleanup_pending`. The existing Lambda MicroVM Workspace
runtime does not yet provision a PR-scoped Workspace binding or expiring
Tenant-scoped Gateway route. A default-off scheduled Lambda invokes the bounded
expiry sweep, but it is not enabled by default and only updates DSQL metadata.
A cleanup worker with retry/backoff transition to `cleaned`, route
authorization, and Preview quota reservation are still required before Preview
can be enabled for user traffic.

When runtime execution is added, it must bind the immutable PR head SHA to an
isolated Workspace, enforce Tenant authorization on every route, respect the
Tenant Preview count/lifetime quota, and make close/merge/expiry cleanup
idempotent. Preview data must not reuse a developer Workspace volume or expose
database/application ports directly. Production deployment remains subject to
the Task 43–46 runtime/Gateway gates.

See [Task 54](../tasks/task-54-preview-catalog-deploy.md).
