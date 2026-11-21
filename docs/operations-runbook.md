# Hako Control Plane and Resource Plane Operations

This runbook defines safe recovery procedures for the current Hako architecture.
It does not authorize AWS changes, production cutover, Secret retrieval, or
resource deletion. Runtime deployment and live MicroVM behavior remain separate
approval gates.

## Safety preflight

Before any account-level operation:

1. Confirm the explicit environment, AWS account ID, Region, Resource Plane ID,
   and change record. Never use the Organizations Management Account for Hako
   workload operations.
2. Confirm every target has the expected `hako:managed-by=hako`,
   `hako:resource-plane-id`, and, where applicable, Tenant/Workspace/Operation
   identity tags. Tags are evidence to investigate; they do not authorize
   deletion or adoption by themselves.
3. Check whether the Control Plane desired state, Resource Plane journal,
   provider state, and audit record agree. If identities disagree, stop and
   preserve evidence; do not adopt or delete an unbound resource.
4. Separate read-only investigation from mutations. AWS apply, restore,
   redrive, cutover, Secret access, and resource deletion require the account
   owner's explicit approval and a scoped change plan.

## Aurora DSQL backup and restore

Aurora DSQL protection is configured through AWS Backup, not a manually copied
database file. The platform owner must define backup frequency, retention,
cross-Region copy, KMS policy, and recovery objectives in the Control Plane
environment before production use.

For a recovery drill or incident:

1. Identify the approved AWS Backup recovery point and record its creation time,
   source cluster ARN, Region, encryption key, and restore job ID.
2. Restore into a **new** DSQL cluster. Do not overwrite or delete the source
   cluster during recovery. AWS Backup restores DSQL recovery points as full
   cluster backups and creates a new cluster; see the [Aurora DSQL backup
   guide](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/backup-aurora-dsql.html)
   and [AWS Backup restore procedure](https://docs.aws.amazon.com/aws-backup/latest/devguide/restore-auroradsql.html).
3. Keep API, Dispatcher, Scheduler, and consumers pointed at the original
   cluster while the restored cluster is validated. Verify cluster identity,
   schema/migration level, critical Tenant/Workspace/Operation counts, recent
   audit records, and application-level invariants using an approved
   read-only validation role.
4. Reconcile any events created after the recovery point from durable queues
   and provider journals. Database restoration does not undo already executed
   Resource Plane side effects. Replays must use the original immutable
   Operation/Run identities and idempotency fences.
5. Change the Control Plane database endpoint only through a reviewed,
   approved deployment/cutover. Preserve the old cluster until the recovery is
   accepted and rollback is no longer required.

AWS Backup restores an entire DSQL cluster; it is not a table-level recovery
procedure. Measure restore time and data-loss window in an isolated drill
before publishing RTO/RPO targets. A backup plan and a successful restore have
not been provisioned or exercised by local repository tests.

## Schema migrations and rollback

Hako schema migrations are forward-only. Do not edit an applied migration or
run an improvised reverse SQL script in a live environment.

1. Record the deployed binary version and last applied migration.
2. Stop or hold new schema-dependent work through the approved release
   procedure; keep already durable Outbox and queue records intact.
3. For a failed migration, use a reviewed forward-fix migration that restores
   invariants and remains compatible with the currently deployed binary.
4. Roll back the application binary only when its compatibility with the
   current schema is verified. Otherwise deploy a forward-compatible fix.
5. Validate migration state, API health, Outbox dispatch, and one read-only
   Tenant/Workspace path before reopening traffic.

Local PostgreSQL tests exercise migrations and transaction behavior, but do
not prove a production DSQL restore or migration rollback. Run migrations with
the dedicated migration identity; application roles do not receive DDL
permissions.

## Transactional Outbox, queues, and DLQs

- An unpublished Outbox row is durable work. Inspect its event type, immutable
  aggregate identity, retry count, and last failure class. Retry the same event;
  do not create a replacement operation to bypass a stuck row.
- A queue age/backlog alarm means delivery is delayed, not necessarily lost.
  Compare Outbox publication state with queue and consumer evidence before
  replaying anything.
- A DLQ message may have partially completed side effects. First inspect its
  operation/run ID, receive history, Resource Plane journal, and current
  provider binding. Never bulk-redrive a DLQ.
- Redrive only an explicitly selected message after verifying that its
  handler is idempotent and its immutable identity is still current. Use the
  queue's audited redrive path; if no approved path exists, leave the message
  in the DLQ and escalate.
- Do not purge published Outbox rows or processed webhook deliveries until a
  retention policy accounts for audit needs, webhook replay windows, result
  idempotency, and operational recovery. No generic Outbox purge job is
  enabled by this runbook.

Resource Plane queue alarms, visibility, redrive policy, and the limits of
manual DLQ redrive are documented in [Resource Plane queues and DLQs](resource-plane-queues-and-dlq.md).

## Workspace and Lambda MicroVM cleanup

1. Request cleanup through the authenticated Workspace/Run lifecycle so Hako
   writes the desired-state transition, tombstone, audit event, and Outbox
   command.
2. Retry the same Operation/attempt. Wait for provider termination and the
   durable journal's confirmed cleanup state before releasing persistent
   bindings or deleting Workspace data.
3. For EFS, preserve the Access Point and Workspace data until the controller
   confirms the expected allocation is terminated and the deletion policy is
   satisfied. Do not manually remove an Access Point or directory to clear a
   stuck Workspace.
4. For an ambiguous create with no provider ID, replay only the journal-pinned
   launch request and idempotency token. Do not infer ownership from a list of
   provider resources.
5. Investigate tagged resources with no matching database/journal binding as
   possible orphans. Export the exact ARNs and tags for review; never issue a
   broad tag-based delete.

See [Workspace Runtime Operations](workspace-runtime-operations.md) for
journal-fenced retry rules. Its provider checks are fake-backed locally; no
live MicroVM discovery or cleanup has been verified.

## Artifact retention and cleanup

- Artifact metadata and Tenant quota are released only after the Resource
  Plane deletes the exact object and the Control Plane accepts the
  expiry/reference-fenced acknowledgement.
- On object-delete or acknowledgement failure, retain the metadata and retry
  the same candidate. The cleanup Worker reports bounded error classes; use
  correlation ID, Run ID, and fixed failure class in logs, never print an S3
  URI or raw AWS/DSQL error.
- Managed artifact storage is opt-in. Verify its prefix, SSE configuration,
  versioning, lifecycle retention, and KMS key policy before enabling the
  cleanup schedule. With S3 versioning, `DeleteObject` may create a delete
  marker; lifecycle must expire noncurrent versions and markers as intended.
- Treat retained usage as metadata accounting, not live S3 inventory or a
  billing measurement. Reconcile inventory only with a separate, approved,
  read-only procedure.

## Recovery drills and evidence

Local procedures must keep the development PostgreSQL service running and use
synthetic credentials/fake AWS providers. Relevant repository targets include
`make test-software-factory-e2e`,
`make test-workload-runtime-integration`,
`make test-webhook-lifecycle-integration`,
`make test-actions-runner-lifecycle-integration`,
`make test-artifact-retention-integration`, and
`make test-workspace-idle-lifecycle-integration`.

Record the command, commit, test result, database target, synthetic/fake
provider scope, and any unverified AWS behavior. These tests do not authorize
AWS apply or external cutover and do not prove live Lambda MicroVM isolation.
