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

This change provides the persistence/domain foundation and state/artifact store
only. It does not yet create runs from Push/PR events, launch a Fake/AWS
Runtime, execute repository code, publish GitHub Checks, upload artifacts, or
expose Workload Run APIs. The next scheduler/consumer must preserve Tenant
authorization, trust policy, bounded timeouts, isolated execution, and
redacted-log requirements before enabling repository-triggered Jobs.
