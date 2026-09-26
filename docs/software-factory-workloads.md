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

## Current scope and follow-up

The persistence/domain foundation and state/artifact store are implemented.
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

`JobRuntime` currently keeps results and redacted logs in a Tenant-checked,
process-local idempotency cache. It is not yet wired to `workload_runs`,
`workload_artifacts`, S3, the GitHub webhook inbox, a scheduler/dispatcher, or
GitHub Checks; process restart therefore loses the cached Job result. Do not
use this local Fake Runtime as a production executor. In particular, deployable
workers still need immutable image verification, durable result/artifact
storage, cancellation cleanup, trust-level/network policy, and end-to-end
authorization before repository-triggered Jobs are enabled.
