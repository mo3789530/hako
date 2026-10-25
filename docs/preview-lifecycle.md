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
`runtime_enabled` remains false in the public list response. The local
provisioning, Guest startup, and cleanup command paths are implemented, but the
Preview runtime is not enabled for user traffic until the default-off
Control Plane/Resource Plane deployment and real MicroVM acceptance gates are
completed.

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

To reach an application port through the same authenticated Gateway, use a
separate loopback-only forward:

```bash
hako github repository previews forward --tenant <tenant> \
  --installation <installation-id> --repository <repository-id> \
  --pull-request <number> [--local-port 0]
```

The CLI binds only to `127.0.0.1`, requests a fresh Preview Session scoped to
`port-forward` for each local connection, and revokes it when that connection
closes. The remote destination is a numeric port on the Preview MicroVM; no
host or public route is accepted. Preview Sessions are restricted at the
Gateway to the application port `3000`; all other ports are denied even if the
general Workspace port policy allows them. The Resource Plane Gateway
composition injects `PreviewPortAuthorizer`, which allows only a valid
generation-specific Preview Workspace ID, the `port-forward` permission, and
port `3000`. The Gateway's existing active-session check verifies that the
ticket still maps to a current, running Preview generation. For ordinary
Workspaces, the Gateway separately asks the Control Plane to authorize only
declared TCP ports of healthy Services in the current runtime generation
(Task 86). This is locally tested; production Gateway rollout remains
unverified.

An opted-in repository must include `.hako/preview.yaml` in the pinned PR
commit:

```yaml
version: 1
command:
  - npm
  - run
  - dev
```

The command is an argv array, runs with `/workspace/src` as its working
directory and a minimal environment (`PORT=3000`, `HOST=127.0.0.1`), and runs
as the unprivileged Guest Agent user. Hako waits until `127.0.0.1:3000` accepts
connections before acknowledging Preview provisioning. Startup failure
compensates by deleting that exact generation's MicroVM. App logs are not
persisted yet. The CLI opens the local URL for the Preview application; no
public application URL or inbound port is created.

The CLI creates a fresh Preview Session for each Gateway WebSocket connection,
uses the logical runtime Workspace ID returned by that API response for
routing, and revokes the Session when the connection closes. The local listener
binds only to `127.0.0.1`. Until the Resource Plane binds the running Preview
generation, the API rejects session creation. The shared Gateway resolves the
private runtime Workspace through its Resource Plane journal; there is no
separately published Preview network route. Terraform requires its image,
journal bucket/prefix, public origin, and Agent ticket signing key to match the
Preview Resource Controller. The `open` command only connects to an already
provisioned generation; it does not create runtime resources.
A local API + TLS/WSS Gateway E2E verifies the CLI's loopback tunnel,
generation-specific Session issuance, byte relay to the authorized Workspace
ID, cancellation, and Session revocation using a fake Guest stream. This does
not create a Preview MicroVM or validate a deployed route.

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
identity so the Resource Plane can recover an ambiguous launch through its
durable runtime journal and the same Lambda MicroVM idempotency request. Lambda
MicroVM has no verified per-VM tag or tag-list resource, so Hako must not adopt
or delete an unjournaled VM by scanning. If neither a persisted provider ID nor
the exact journaled launch request can prove ownership, cleanup stays pending
for operator reconciliation. A compensation failure leaves the claim for
lease-based recovery instead of pretending cleanup succeeded. If a successful
result loses its generation fence, it also invokes idempotent compensation. PostgreSQL lifecycle
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
syntax, keeping it compatible with Aurora DSQL. The Outbox Dispatcher has a
dedicated Preview provision/cleanup queue route with strict generation, claim,
logical identity, and Plane validation; it never falls back to the Workspace
command queue. A default-off Control Plane scheduled producer now runs the
transactional provision and cleanup claimers and writes only fenced Outbox
commands. Its dedicated DSQL role can reserve capacity and enqueue commands,
but cannot update provider IDs or call AWS/Resource Plane APIs. Dispatch to a
Plane still depends on the existing Dispatcher schedule and a configured
Preview queue URL. PostgreSQL integration covers Tenant region/Plane filters,
full-capacity exclusion, pinned placement, and capacity release after cleanup.
The Resource Plane queue/provider composition has since been added locally as
described in Runtime and cleanup status below; deployment and real MicroVM
verification remain open.

The accepted Preview Outbox envelope and current runtime status are
documented in [Preview Runtime command routing contract](preview-runtime-command-contract.md).

Migrations 000172–000175 add an optional Tenant GitHub App identity pin to each
Preview runtime generation. When a new generation is recorded, Hako resolves
an active, approved, Tenant-owned App integration bound to that repository and
stores its integration ID, config revision, and credential generation. A
retry keeps the original pin; a later PR head generation resolves and records
the then-current eligible integration. Shared Hako credentials are never used
as a fallback. If no eligible Tenant App exists, the generation remains
unpinned and must fail closed for source checkout. Existing unpinned rows are
not silently backfilled, so enabling checkout for them requires a new
generation or an explicit migration policy. Broker checkout, source transfer
into the MicroVM, and Guest startup are now composed locally. The migrations
have not been applied to AWS.

The Tenant App broker now has a separate mTLS-only
`POST /v1/preview-checkout/token` endpoint for Resource Plane certificates on
its explicit checkout allowlist. It accepts only the Preview logical identity,
generation, claim/attempt, and pinned head SHA. The broker resolves the App
pin from the private allocation row and issues a Contents-read installation
token only while the same generation is current, unexpired, and held by the
calling Plane's active provisioning lease. It rechecks the claim and App
credential after the GitHub token exchange before returning the token, and the
response is marked `no-store`. Missing or stale pins fail closed. The endpoint
returns the broker-validated Repository owner/name with the token, and the
Resource Plane has a bounded HTTPS client for that response. The client sends
no Resource Plane ID in its body; the server derives it from the mTLS URI SAN.
This is prepared for checkout. The
`previewcontroller.PreviewSourceMaterializer` composes the provider grant
with the existing immutable Git checkout and S3 source archive stager. It pins
the exact PR head SHA, stores the archive under the runtime Workspace,
generation, and active claim identity, rejects a returned prefix outside that
identity, and removes the temporary checkout tree. It never executes
repository code. The Lambda MicroVM Resource Controller now requires this
materializer and a source installer whenever its Preview queue is configured.
After the exact runtime binding is resolved, it reads only that attempt's S3
archive and transfers it over the Guest's one-use, file-transfer-ticket WSS
endpoint. The Guest verifies the archive SHA-256, extracts only regular files
and directories, and atomically installs it at `/workspace/src`; an idempotent
retry is accepted only when the existing tree has the same deterministic
archive digest. Other destinations and invalid digests are rejected. Local
tests cover archive staging, protocol validation, Guest installation, retry,
conflict, and the controller source-install sequence.

The Resource Plane controller's Preview composition reads four distinct,
explicit Secrets Manager ARNs (broker mTLS certificate/key/CA and Guest ticket
signing key), stages and reads archives under one configured S3 bucket prefix,
and uses the configured Lambda MicroVM image for VM lookup/auth-token scope.
Terraform models these settings and exact-prefix/secret IAM permissions in the
default-off `preview_runtime_source` object. No Secret was retrieved and no
Terraform apply was performed. The deploy gate still requires the matching
ticket public key and file-transfer endpoint in the immutable Guest image,
Resource Plane egress/connectivity validation, Gateway routing/publication,
and an end-to-end real MicroVM test. The resource adapter installs the source
and starts the repository-declared Preview process. Gateway authorization is
composed locally: Preview is restricted to its fixed port, and ordinary
Workspace forwarding requires a healthy current-generation Service Catalog
port (Task 86). Resource Plane egress validation, the deployed Gateway path,
and real MicroVM validation remain open. The app is reached through the
loopback-only CLI tunnel, not a public application URL.

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

The public Preview feature remains disabled: metadata/list responses report
`runtime_enabled: false`, and Session creation requires a current running
provider binding. The local execution path is implemented beyond lifecycle
metadata. A default-off scheduled producer claims current generations, reserves
and pins Resource Plane placement, and writes provision/cleanup commands to
the Control Plane Outbox. The normal Outbox Dispatcher routes those commands
to the exact pinned Plane queue. The Resource Plane Lambda has a strict Preview
queue handler and composes the existing durable Lambda MicroVM Workspace
runtime journal with Preview generation fencing. It uses stable ensure/delete
revisions and resolves the MicroVM only through that journal; it does not scan
for or adopt unjournaled MicroVMs.

Provisioning checks out the exact pinned PR head using the Tenant App
credential pin and mTLS broker, stages a credential-free archive in the
configured S3 prefix, installs it at `/workspace/src` over the Guest's one-use
file channel, and starts the strict `.hako/preview.yaml` command. The app is
health-checked on loopback port 3000 before provision success is reported. The
Gateway uses the logical generation Workspace ID and resolves it through the
same journal; no separate public Preview network route is published. The CLI
connects through a loopback-only tunnel.

Cleanup claims are generation-fenced and retryable. The Resource Plane
controller resolves and deletes the exact journaled MicroVM, and cleanup is
acknowledged only after provider confirmation. Unjournaled resources are never
discovered by scanning. Quota remains reserved until all generations are
confirmed cleaned. Local PostgreSQL/fake-provider tests cover provisioning,
source installation, app startup, partial-create compensation, retries,
generation changes, cleanup, and result replay.

The scheduled producer, result consumer, command/result queues, IAM grants,
Guest ticket-key agreement, source broker/S3 access, and Gateway configuration
have default-off deployment contracts. No AWS resources or DSQL grants have
been applied, no real Secret was retrieved, and the pinned ARM64 image has not
been exercised in a Lambda MicroVM. Therefore this local execution path is not
yet enabled for user traffic; real AWS networking, Gateway routing, app
health, and physical cleanup remain unverified.

A separate default-off scheduled Lambda also invokes the bounded metadata
expiry sweep. It only changes lifecycle state; the provision/cleanup producer
and Resource Plane command path perform the later runtime work.

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
