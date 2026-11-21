# Workspace Reconciler

`cmd/hako-reconciler` periodically expires stale Operations and compares each Workspace's Desired State with its Observed State. For a bounded batch of mismatches without a pending/running Operation, it creates a corrective Operation, an `operation.reconciled` event, and an `operation.requested` Outbox command in one database transaction. The existing Dispatcher then publishes that command to the Workspace's placed Resource Plane.

## Stale Operation timeout

Before listing state mismatches, the Reconciler marks up to `BatchSize` pending/running Operations with `updated_at` older than `HAKO_RECONCILER_OPERATION_TIMEOUT` as failed with `error_code=operation_timeout`. The default timeout is `30m`; the value must be at least one second. The transition and an ordered `operation.timed_out` event (including the cutoff) commit atomically. Compare-and-swap predicates make concurrent Reconciler instances safe; a result or another timeout worker that wins first is not overwritten. The Workspace Observed State is deliberately left unchanged because an expired Operation does not prove whether the Resource Plane performed the work. The terminal failure enters the ordinary `HAKO_RECONCILER_FAILURE_DELAY` cooldown before corrective work can be issued.

Expiry does not cancel an SQS message or stop a Runtime process. The Resource Controller rejects a command that has not started within `HAKO_COMMAND_MAX_AGE` (default `30m`) and reports `operation_expired` without invoking the Runtime. Set this age no greater than `HAKO_RECONCILER_OPERATION_TIMEOUT`, so the result can either fail the active Operation or be ignored if the Reconciler already expired it. Schema v2 commands carry a monotonically increasing Workspace revision; the Control Plane ignores results for superseded revisions, and the Fake Runtime rejects stale/duplicate revisions within its process lifetime. A command accepted just before timeout may nevertheless continue after Control Plane expiry. The production Runtime's durable, atomic fence (including delete tombstones), in-flight cancellation, and compensating cleanup remain unimplemented; do not treat the current fence or age cutoff as proof of exactly-once execution.

## Desired/Observed mapping

| Desired State | Observed State | Corrective Operation |
| --- | --- | --- |
| `running` | `running` | none |
| `running` | `suspended` | `resume` |
| `running` | any other non-`running` state | `ensure_running` |
| `suspended` | `suspended` | none |
| `suspended` | any other state | `suspend` |
| `deleted` | `deleted` | none |
| `deleted` | any other state | `delete` |

This is a conservative MVP mapping. Runtime-specific preconditions and partial-resource cleanup are not evaluated here; the Resource Controller must make commands idempotent and report the actual outcome.

## Duplicate prevention and retry delay

- Workspaces with a pending/running Operation are not candidates.
- A terminal Operation updated within `HAKO_RECONCILER_FAILURE_DELAY` suppresses immediate corrective work. The default delay is `1m`; after it elapses, persistent drift can produce a new Operation with a new idempotency key.
- The store re-reads Desired/Observed State and Placement in the write transaction, then increments `workspace_status.reconcile_revision` with a compare-and-swap guard before writing Operation/event/Outbox. The resulting revision is carried by schema v2 commands/results; direct create initializes revision 1 and user actions increment it as well. Legacy rows with a null revision are treated as revision zero and initialized on first claim. Concurrent Reconciler processes cannot both claim the same status revision.
- `workspace_status.updated_at` is refreshed when a reconciliation revision is claimed, even though Desired/Observed values stay unchanged.

The revision migration adds a nullable `BIGINT` and initializes it lazily. Aurora DSQL's documented `ALTER TABLE ADD COLUMN` syntax is narrower than its `CREATE TABLE` column constraints, so the application handles existing null values instead of relying on an `ALTER ... ADD COLUMN ... DEFAULT/NOT NULL` rewrite ([Aurora DSQL ALTER TABLE support](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/alter-table-syntax-support.html)).

The retry delay prevents a tight loop but is not a retry budget. Maximum attempts, exponential Operation retry scheduling, a Resource Plane DLQ/RedrivePolicy, and compensating cleanup for partially created real resources remain separate work.

## Running locally and limits

The Reconciler uses `HAKO_DATABASE_URL` (local PostgreSQL) or `HAKO_DSQL_HOST` (Aurora DSQL), with AWS SDK default credentials for DSQL IAM authentication. Run it with:

```sh
go run ./cmd/hako-reconciler
```

`HAKO_RECONCILER_INTERVAL` defaults to `10s`; `HAKO_RECONCILER_FAILURE_DELAY` defaults to `1m`; `HAKO_RECONCILER_OPERATION_TIMEOUT` defaults to `30m`. It only updates Operation control state and creates Outbox commands. It does not execute AWS APIs or infer/change Workspace Observed State on timeout. A Resource Plane consumer/controller must execute commands and report transitions; until then, the Dispatcher can deliver queued work but Workspace state will not converge.
