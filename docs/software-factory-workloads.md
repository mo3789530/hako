# Software Factory Workload Runs

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
mismatch, and cancellation-before-start.

The artifact byte store is still an injected interface; a concrete S3 writer
is available as `S3ArtifactWriter`. It stores content-addressed objects under
`<prefix>/<tenant>/<run>/logs/<sha256>.txt`, uses S3 checksum validation, and
defaults to SSE-S3; an optional KMS key enables SSE-KMS. The bucket must remain
private and its lifecycle/retention policy must be configured by the operator.
The writer uses the AWS SDK default credential chain, so deployed workers
should receive only a scoped IAM role (`s3:PutObject` on the configured prefix,
plus KMS permissions when enabled). No credentials, ACLs, or presigned URLs
are embedded in artifact metadata.

A deployed Workload worker/handler is not implemented. The standalone local
`JobRuntime` also keeps a process-local cache, so it is not a durable queue or
production executor. Before repository-triggered Jobs are enabled, deployable
workers still need image digest verification, bucket/IAM provisioning,
in-flight cancellation cleanup, trust-level/network policy, and end-to-end
authorization.
