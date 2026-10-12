# Software Factory Workload Runs

## Repository webhook verification Jobs

An explicitly enabled Tenant repository can create a fixed, untrusted
verification Job from authenticated `push` and pull-request
`opened`/`reopened`/`synchronize` webhook deliveries. The repository setting is
default-off and is managed by a Tenant Owner/Admin through the GitHub repository
factory API/CLI. The server selects `kind=job`, `trust=untrusted`,
`runtime_class=standard`, and a 30-minute timeout; event SHA/ref are read from
the normalized, signature-verified inbox record. The Run, queued Check,
Outbox request and delivery `processed` transition commit atomically. A
repository opt-in timestamp suppresses pre-opt-in queued deliveries. Branch
deletion pushes and unsupported pull-request actions are consumed without
creating a Run.

This trigger path is separate from GitHub Actions `workflow_job` handling and
does not run arbitrary repository workflow steps. It remains feature-flagged
off until the Check consumer and Lambda MicroVM-backed Workload executor are
deployed and monitored. See [GitHub Webhook Processor](github-webhook-processor.md)
for enablement and remaining operations work.

## Webhook delivery idempotency

`workloads.CreateOrGetFromWebhookDelivery` is the durable persistence boundary
for an already-authorized repository trigger. It reloads the normalized event
from the integration-scoped inbox, rechecks the current Tenant App and
Installation authorization, and derives Installation, Repository, ref, and
commit SHA from that stored event rather than caller-supplied queue fields.
Only push, pull-request opened/reopened/synchronize, and
`workflow_job.queued` event types are eligible. The caller must still perform
trigger, workflow provenance, Tenant trust/runtime and capacity policy checks
before invoking it.

The idempotency key is `github-delivery-` plus the SHA-256 digest of the
integration ID, a NUL separator, and the original GitHub Delivery ID. Run
creation, initial GitHub Check metadata and its Outbox event remain in the same
transaction. Replaying a delivery with identical policy returns the same Run;
changing the trust/runtime/request fingerprint for that Delivery ID conflicts.
This helper does not schedule a runner or make `workflow_job` execution
available by itself. Task 52 must bind authoritative workflow/job provenance
and pass policy before any runtime is provisioned.

`Workspace` is a long-lived interactive environment. A Software Factory
execution is a separate, short-lived `WorkloadRun` with kind `job`, `agent`, or
`preview`; it does not reuse the Workspace `operations` table, whose foreign
keys and lifecycle are Workspace-specific.

## Persisted model

- `workload_runs` stores Tenant-scoped identity, optional registered GitHub
  repository and immutable commit SHA, runtime class, trust level, placement,
  desired/observed lifecycle state, idempotency fingerprint, revision, timeout,
  and timestamps.
- A composite foreign key ties a run's repository to the same Tenant and
  GitHub Installation in `tenant_github_repositories`. The API/dispatcher must
  still apply Tenant authorization and trust policy; `CreateOrGet` also rejects
  new repository runs unless the matching Installation is active and not
  suspended. Existing idempotent retries return their original run even if
  the Installation is subsequently suspended.
- `workload_run_events` stores ordered state-transition metadata. It does not
  accept arbitrary event payloads, source content, or logs; callers append only
  Hako-owned event names.
- `workload_artifacts` stores Tenant/run-scoped object references, byte length,
  SHA-256, retention expiry, and redaction status. Artifact bytes live in an
  external store such as S3/ECR, not DSQL. A `logs` artifact is rejected unless
  it is marked redacted, and presigned/query-bearing storage URLs are rejected.

Migrations `000046`–`000049` create these tables and the timeout scan index.
Before deploying any API/consumer that reads or writes the new tables, rerun
the `hako_api` table grant in
`infra/terraform/modules/control-plane/bootstrap-api-role.sql.tmpl` as the
DSQL migration admin; DSQL does not automatically grant privileges on tables
created after the initial role bootstrap.

## Lifecycle contract

Every run starts `pending` with desired state `running`. Workers move it
forward through `queued`, `provisioning`, and `running`, then to exactly one
terminal state: `succeeded`, `failed`, `cancelled`, or `timed_out`. The store
uses a monotonically increasing revision as compare-and-swap protection;
terminal states cannot transition back to active states.

Cancellation is a desired-state request: it sets desired state to `cancelled`
and emits one `workload.cancel_requested` event. The executor acknowledges it
by transitioning observed state to `cancelled`. A bounded `ExpireDue` sweep
marks runs past their immutable deadline `timed_out`, sets desired state to
cancelled, and records the terminal event. The default timeout is 30 minutes;
requests select a timeout duration in whole seconds up to 24 hours, and the
server derives the immutable deadline from the original creation time. The
duration, not the derived timestamp, participates in the idempotency
fingerprint so retries with the same key remain stable. Tenant quotas and
per-kind policy limits remain the responsibility of the future API/scheduler.

The local Go-test execution service handles the expected in-flight cancellation
revision change: after the executor confirms it has stopped, it reads the latest
Run revision and transitions a still-running Run with desired state `cancelled`
to terminal `cancelled`, without attempting to persist a success artifact. A
different unexpected revision change remains an OCC conflict. This local
PostgreSQL test does not prove that a deployed Worker receives the cancel
command, stops the Lambda MicroVM allocation, or completes cleanup; API
cancellation remains asynchronous until those worker lifecycle gates pass.

`CreateOrGet` scopes idempotency to a Tenant and compares a normalized request
fingerprint. Reusing a key for a different request is a conflict. Repository
runs require an exact lowercase 40- or 64-character commit SHA; mutable branch
names are metadata only and do not select the source revision.

## Workload Run API visibility

Authenticated routes expose paginated list, detail, cancellation, and an
opt-in create endpoint under `/v1/tenants/{tenant_id}/workload-runs`. Tenant Owner/Admin roles can
read all Runs in that Tenant; other members can read only Runs whose
`requested_by` is their Hako User ID. Missing Tenant membership is returned as
404, as are Runs outside the caller's visibility. Cancellation sets the
desired state and appends an immutable Run event; it does not synchronously
stop an already-running executor until a worker cancellation channel is wired.
Run creation accepts only a registered active Tenant GitHub Installation and
Repository plus an immutable lowercase commit SHA. Kind (`job`), trust
(`untrusted`), and runtime class (`standard`) are server-selected. The API
atomically writes the pending Run, a `workload_run.schedule_requested` Outbox
event, and an audit record; the event carries only schema version, Tenant ID,
and Run ID. Idempotent retries return the existing Run without adding another
event. `HAKO_WORKLOAD_RUNS_ENABLED` defaults to false. Terraform's
`enable_workload_run_creation` also defaults to false and requires the Outbox
Dispatcher/Workload queue, but this alone is not sufficient to enable it:
there is not yet a deployed consumer/runtime executor, so Runs would remain
pending in the scheduler queue. Keep the setting disabled until that consumer
is deployed and monitored.

The CLI mirrors these routes as `hako workload create --tenant <id>
--installation <id> --repository <id> --commit <sha> [--ref <ref>]
[--timeout-seconds <seconds>] [--idempotency-key <key>]`,
`hako workload list <tenant-id> [limit] [offset]`,
`hako workload show <tenant-id> <run-id>`, and
`hako workload cancel <tenant-id> <run-id>`. Create defaults to an untrusted
Job with a 30-minute timeout and a generated idempotency key; if the command
fails after submission, retry with the printed key. List output is a
tab-separated summary; `show` returns the full JSON Run. CLI cancellation
reports acceptance, not that the executor has already stopped.

## Current scope and follow-up

Task 38 adds `internal/repositorycheckout`, a worker-side provider that first
resolves the canonical owner/repository through the active Tenant Installation
binding, then obtains a short-lived token and fetches only the persisted
lowercase commit SHA. The user-supplied `ref` is metadata and never selects the
revision. Git receives the token through a private askpass file descriptor; it
does not appear in Git arguments, environment, remote configuration, output,
or the returned checkout result. The temporary directory is mode `0700` and
the caller receives an explicit cleanup handle. Fetches are shallow and capped
at two minutes with a 512 MiB pack-file limit and 1 GiB total worktree limit.
Redirects, hooks, submodules, and Git LFS are disabled or rejected. Repositories
using submodules/LFS and sources above these limits are unsupported. Checkout
is not yet connected to the Workload handler or deployed worker; Tasks 39/40
own that integration and deployment.

The persistence/domain foundation and state/artifact store are implemented.
`internal/workloadjobs.SchedulerConsumer` now consumes the versioned SQS
schedule envelope, extends message visibility while a handler runs, and only
deletes a message after the handler reports a durable outcome. Malformed or
failed deliveries remain on the queue for retry and configured DLQ redrive.
Delivery is at-least-once, so the eventual handler must be idempotent by
Tenant/Run ID; a database commit followed by a failed SQS delete will be
redelivered.
The SQS worker role will require receive/delete/change-visibility permissions
scoped to the scheduler queue. This is consumer infrastructure, not yet a
deployed worker or a handler wired to repository checkout and execution.

The Control Plane now has a result application handler and an injectable SQS
consumer for the versioned
Resource Plane result envelope. It validates Tenant, Run, attempt, generation,
placement, and commit against DSQL; stale generations are acknowledged without
state changes, and terminal duplicates are idempotent. Cancellation requests
and the immutable `timeout_at` win over a late success. A result can carry only
redacted log artifact metadata (opaque storage reference, size up to 100 MiB,
and SHA-256); artifact metadata and the terminal Run transition commit in one
transaction. The consumer defaults to batch size one and deletes a receipt only
after durable apply, duplicate, or stale classification; malformed and
transiently failed deliveries remain for retry/DLQ. It is not yet wired into a
deployed service or queue configuration, and physical runtime cleanup remains
a separate unfinished protocol.

The Resource Plane dispatch envelope contains identity only
(`tenant_id`, `workload_run_id`, `attempt_id`, `generation`,
`resource_plane_id`, and `idempotency_token`). It intentionally omits
`timeout_at` and execution parameters: these must be resolved from a trusted
Control Plane source before launch. A wire-contract test verifies that the
Control Plane marshalled command is accepted by the Resource Plane's strict
decoder.

`internal/workloadjournal` provides a Resource Plane S3 journal primitive.
Creation uses `If-None-Match: *`; updates require the previously observed ETag
with `If-Match`, and S3 409/412 conflicts map to a retryable journal conflict.
The journal keeps immutable attempt identity and independent execution and
cleanup state, and rejects advancing to launch/run after cancellation. It is a
storage/state-machine foundation: the Task 62 command Acceptor now uses it,
and Tasks 63–65 add a controller/official AWS SDK adapter. `ListRecoveryPage`
can scan a bounded page under an explicit journal prefix, validate each key
and payload, and return only attempts that may need reconciliation. The
controller's `RecoverPage` replays those identities through the same Poll path
used by normal delivery and isolates per-attempt failures. These are recovery
primitives, not a production recovery scheduler: no distributed lease,
schedule, alarm, IAM policy, or runtime composition is wired. A scheduled sweep
must restart at page one periodically so transient failures are retried; a
continuation token is only for finishing the current bounded scan. A successful
conditional write is not an atomic transaction with a runtime API.

`internal/workloadcontroller.Acceptor` validates the versioned identity-only
dispatch envelope and configured Resource Plane before conditionally creating
the journal. Duplicate deliveries reload the saved checkpoint and require an
exact Tenant/Run/attempt/generation/placement/token match and a valid ETag.
They preserve cancellation and cleanup state. Storage errors are returned for
retry rather than reported as success. Acceptance does not execute a Job.
The local recovery-page primitive can replay an accepted journal, but Task 40
must deploy and monitor durable recovery before this becomes an acknowledgement
boundary in a deployed command consumer.

`internal/workloadcontroller.LambdaHandler` is the SQS-triggered Lambda
adapter for this command path. It reads `HakoEventType` only from the trusted
Dispatcher message attribute, passes the body to the controller's strict
identity decoder, and reports individual failures through Lambda's partial
batch response. A malformed/unsupported command or transient journal/provider
failure is retried; a command is not acknowledged merely because JSON decoded.
This adapter is unit-tested but is not yet composed with production resolver,
agent, runtime, IAM, or event-source mapping, so workload creation remains
disabled pending Task 40 readiness.

The local Fake Runtime also has an explicit Go test Job entry point for an
already-checked-out repository and immutable commit. It verifies `HEAD` before
execution and invokes `go test ./...` only inside a one-shot Podman container
with networking disabled, a read-only repository, a read-only optional module
cache, no host environment forwarding, dropped capabilities, resource limits,
and a non-root UID. The bounded `/tmp` tmpfs is executable because `go test`
must launch test binaries there; the repository and container root filesystem
remain read-only. Logs are size-limited and common GitHub/AWS/Bearer/JWT token
forms plus caller-provided secrets are redacted before a result is exposed.
The runtime is injectable in tests so the container policy and commit check
can be validated without executing repository code on the host.

`internal/workloadjobs.Service` connects the Fake Runtime to persisted
`workload_runs` state and `workload_artifacts` metadata. It refuses a cancelled
or non-pending run, checks the commit against the persisted run, uses the
remaining immutable run timeout, and atomically records the terminal state and
redacted-log metadata after an `ArtifactWriter` stores the bytes. PostgreSQL
integration tests cover successful/failed completion, metadata, commit
mismatch, cancellation-before-start, timeout completion, and artifact-store
failure (which records a safe terminal failure without creating metadata for
an upload that did not succeed). The Task 42 Factory E2E additionally runs
both a successful and a failing Go test from signed Tenant webhook ingress,
checks redaction and success/failure GitHub conclusions, and retries a
synthetic GitHub 503 without re-running either Job. The Worker remains a local
Fake Runtime path; this E2E is not evidence of Lambda MicroVM execution.
The same E2E sends a third signed delivery, persists a pre-dispatch cancellation,
and passes the scheduler message through `DispatchHandler`; it verifies no
placement or attempt is created and GitHub receives a cancelled Check. A
fourth signed delivery starts a local executor, persists an in-flight cancel,
waits for executor stop, and delivers the resulting cancelled Check through
the fake GitHub consumer. This validates the local database/OCC and Check path,
not delivery of a cancellation command to a deployed Worker, MicroVM stop, or
physical cleanup. Fifth and sixth signed deliveries inject a timeout and an
artifact-store outage through the same Run/Check path and verify the `timed_out`
and `failure` conclusions. The artifact outage remains fail-closed and creates
no artifact metadata. These two cases use deterministic fake executor/storage
results; they do not simulate a real Lambda MicroVM deadline or S3 outage.

The artifact byte store is still an injected interface; a concrete S3 writer
is available as `S3ArtifactWriter`. It stores content-addressed objects under
`<prefix>/<tenant>/<run>/logs/<sha256>.txt`, uses S3 checksum validation, and
defaults to SSE-S3; an optional KMS key enables SSE-KMS. The bucket must remain
private and its lifecycle/retention policy must be configured by the operator.
The writer uses the AWS SDK default credential chain, so deployed workers
should receive only a scoped IAM role (`s3:PutObject` on the configured prefix,
plus KMS permissions when enabled). No credentials, ACLs, or presigned URLs
are embedded in artifact metadata.

A historical runtime design is recorded in [ADR 0006](adr/0006-workload-worker-substrate.md),
but its EC2 choice has been superseded and EC2 is out of scope. The runtime is
Lambda MicroVM per [ADR 0008](adr/0008-non-ec2-workload-runtime.md). Task 61 must
validate image/container/network capabilities in AWS. Tasks 63–65 now implement
controller/SDK adapter, CP cancel Outbox, cleanup and durable result replay libraries;
Task 40 composes and deploys the remaining agent/queue/recovery dependencies. The CP dispatch/result fencing, S3 journal, and
short-lived queue handoff remain substrate-neutral. SQS visibility renewal
cannot hold a receipt for the API's full 24-hour deadline, and the Resource
Plane executor must not connect directly to Control Plane DSQL.

A deployed Workload worker/handler is not implemented. The standalone local
`JobRuntime` also keeps a process-local cache, so it is not a durable queue or
production executor. Before repository-triggered Jobs are enabled, deployable
runtime-specific workers still need image/artifact verification, bucket/IAM
provisioning, in-flight cancellation cleanup, trust-level/network policy, and
end-to-end authorization. Workload creation remains default-off.
