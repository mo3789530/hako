# Task 50: Security, Recovery, and Production Operations

残作業の実行単位：[Task 68](task-68-webhook-abuse-controls.md)。親タスクは集約用に保持し、子タスク完了だけで配備済みとはしない。

Status: In Progress (local code/dependency review, pinned OAuth authorization, credential-redirect rejection and CI govulncheck implemented; production gates depend on Tasks 40, 42 and 49). Priority: P2.

Progress (2026-10-24): API requests receive a server-generated `req_…`
correlation ID returned in `X-Hako-Request-ID`, added to request context, and
included in structured access logs with route template/status/duration. A
client-supplied ID is never trusted. GitHub webhook ingress carries this ID
through the authenticated inbox; repository-triggered Workload Runs and
platform Actions Runner Checks persist it, include it in transactional Check
Outbox messages, verify it at delivery, and record it in structured Check
delivery logs. Workload Run creation also copies the ID into the Scheduler
Outbox; authenticated Resource Plane execution metadata returns it, and the
Worker carries it in the Lambda MicroVM run hook and signed Agent execution
binding. Artifacts and terminal result events retain and verify the same ID;
the result handler logs it and artifact metadata resolves it from its owning
Run. Raw application output is not modified to inject the ID.

Progress (2026-10-24): Resource Plane Workload dispatch/cancel envelopes now
carry the persisted request correlation ID through Dispatcher validation to
the Lambda SQS handler. Failed command records and Workload result-consumer
deliveries emit structured failure-class logs with validated correlation and
execution identifiers when available; raw provider/database errors and queue
payloads are intentionally omitted. API access logs already include the
server-generated request ID and response status. Focused tests verify the ID
is present on valid failed records and raw errors are not copied into logs.

Workspace Operation create/action requests now copy the server-generated API
request ID into their transactional Outbox command. Dispatcher validates the
optional ID, the Resource Plane controller carries it into terminal results,
and Operation result-consumer failures log it with safe resource identifiers.
Before applying a result, the Control Plane compares any supplied ID against
the original immutable Operation event, preventing a Resource Plane result
from being attributed to another valid request ID.
PostgreSQL API lifecycle tests verify create and suspend commands match their
`X-Hako-Request-ID`; unit tests verify controller result propagation and
failure-log redaction. Legacy commands without a correlation ID remain
accepted. Failure-path correlation is now implemented for Workload and
Workspace Operations. Workspace Gateway authorization/dial/recheck failures
now log trusted Ticket/Tenant/Workspace/Plane identifiers and a bounded failure
class, and artifact cleanup failures log Run/artifact/correlation identifiers;
neither includes raw errors or storage references. Tests cover active Session
recheck failure and S3 cleanup failure. The local Gateway/cleanup failure-path
inventory is now documented in the [Security review](../docs/security-review.md).
Production log retention, alert delivery, and escalation review remain. The
Workspace Service SQS adapter now logs a fixed failure class and AWS message ID
without logging the command body or raw runtime/provider errors; a regression
test checks this redaction boundary. The Resource Plane consumer also maps
Guest and Control Plane report failures to one fixed retry error so an
upstream caller cannot accidentally log raw error details.

## Goal

Close cross-cutting security and operational gaps before production or broader Tenant onboarding.

## Work

Review evidence and remaining risks: [Security review](../docs/security-review.md).

- Review Tenant isolation, Hako RBAC, session replay, duplicate/out-of-order Operations, webhook replay, queue poison messages, secrets, artifact access, and untrusted fork PRs.
- Define DSQL backup/restore, migration rollback/forward fix, Outbox retention, artifact retention/deletion, Workspace cleanup, orphan AWS discovery, and DLQ redrive procedures.
- Add structured correlation IDs from webhook/Operation/Run through Queue, Runtime, logs, artifacts, and GitHub Checks; redact secrets and minimize developer data in logs. Workload/Operation/Gateway/artifact-cleanup failure paths are locally covered; remaining verification is the wider cleanup failure inventory and production log/alert review.
- Establish metrics/SLO alarms for API, DSQL, Outbox lag, queue age/DLQ, worker capacity/failure, Gateway, Runtime, and cleanup backlog.
- Record deferred BYOC, multi-region, persistent database, and billing requirements as independent ADRs with owners and non-goals.

## Acceptance criteria

- Threat-model review has explicit assets, trust boundaries, abuse cases, mitigations, and accepted residual risks.
- Recovery/cleanup drills run against disposable local/fake data and prove idempotence; AWS drills require explicit approval and isolated accounts.
- CI includes Go tests, PostgreSQL integration tests, Podman-isolated workload tests, Terraform fmt/validate, and security/policy checks.
- Operator runbooks include alerts, diagnosis, safe retry/redrive, restore, rollback, and escalation instructions.

## 2026-10-24 Artifact cleanup error boundary

Artifact retention failures now return only bounded sentinel errors (listing,
invalid oversized batch, or one-or-more item failures). The worker records
fixed failure classes with Run/artifact correlation in structured logs, while
provider/DSQL errors, Artifact IDs, and S3 references are absent from the
error returned to the Lambda runtime. Tests exercise list, object-delete,
metadata-release, and oversized-list failures. This closes the Artifact
cleanup response/log boundary locally; Gateway-wide inventory and production
log/alert review remain.

The operator runbook [Control Plane and Resource Plane Operations](../docs/operations-runbook.md)
now defines the non-destructive DSQL backup/restore sequence, forward-fix
migration policy, Outbox/DLQ handling, Workspace/MicroVM cleanup fencing,
tagged-orphan investigation, Artifact retention, and local recovery-drill
evidence. It does not perform or authorize AWS restore, migration cutover,
redrive, or deletion. A real AWS Backup restore and operational drill remain
unverified.

## 2026-10-24 Control Plane monitoring progress

The Control Plane Terraform module now defines HTTP API stage alarms for 5xx
responses and p95 latency, plus Aurora DSQL alarms for query timeouts and
sustained OCC conflicts. Alarm names are exported. The Outbox Dispatcher emits
the indexed oldest-unpublished age in low-cardinality Embedded Metric Format,
with a conditional five-minute lag alarm. The outbox age lookup and Terraform
configuration passed focused PostgreSQL integration, Go unit, OpenTofu
format, and OpenTofu validation checks. `docs/control-plane-terraform.md`
records dimensions, initial thresholds, missing-data behavior, and the fact
that notification actions require environment-specific configuration. End-to-
end alarm notification wiring and production log/alert review remain open.

The wider local failure-log inventory also found outer process logs that copied
raw errors from the Outbox Dispatcher, webhook processor, Operation result
consumer, and Reconciler. Those logs now retain fixed failure classes and
bounded counters/resource-plane identity; Lambda handlers return static retry
errors while internal consumers keep their bounded identity-aware diagnostic
records. API 500 responses use generic HTTP status messages. Workspace Gateway
and artifact cleanup paths were reviewed: runtime server errors are normalized,
cleanup logs use fixed classes and trusted IDs, and storage references/provider
errors are not written. Local process-log changes are covered by bounded-error
handler tests; deployed log retention, alarm delivery, and escalation review
remain.

The remaining Task 50 Gateway/cleanup inventory also covered startup boundaries:
Resource Controller Lambda initialization and Workspace Gateway process-exit
logs now emit fixed failure classes without formatting the underlying error.
This prevents AWS SDK endpoint/configuration details and filesystem paths from
being copied into CloudWatch. Preview cleanup has generation-fenced retry
orchestration but no provider-backed deletion executor yet, so there is no
MicroVM/route deletion log path to claim as implemented; the executor remains
Task 54 work. Production retention, alert
delivery, and escalation review remain unverified.

The follow-up process-wide scan removed raw startup errors from API, Dispatcher,
Webhook Processor, result consumer, Reconciler, GitHub Checks consumer, GitHub
broker, Actions Runner, Workload Controller, Workspace Service Controller, and
local Resource Controller entrypoints. Preview expiry and Workspace idle
scheduled handlers also return fixed retry errors while their logs retain only
bounded failure classes and counters. Focused tests assert that synthetic DB
error details do not enter those scheduled-handler logs or return values; the
affected command packages pass. A source scan found no remaining `log.Fatal(err)`,
`log.Fatalf(...%v...)`, or `log.Printf(...%v...)` patterns under `cmd/`.
This does not verify deployed CloudWatch retention or alert operations.

## 2026-10-25 Go standard-library security update

The pinned `govulncheck@v1.8.0` run found reachable GO-2026-6809 in Go
1.27.2 `crypto/x509` (`Certificate.Verify`). The repository toolchain was
updated to Go 1.27.3, including the Dev Container, Lambda build images, pinned
ARM64 Guest build image digest, CI Podman image, and local test fixtures.
`govulncheck@v1.8.0 ./...` now reports no reachable vulnerabilities;
`go test -p=1 ./... -count=1`, the Podman/PostgreSQL Software Factory E2E,
and `go vet ./...` pass under Go 1.27.3. Five dependency advisories remain
unreachable according to govulncheck. The updated images have not been
deployed.

The migration command now logs only the fixed `failure_class=migration` on
startup/DDL failure; it does not include the underlying database error, SQL,
or connection details. A regression test supplies a sentinel error and verifies
that it is absent from the structured log. Local migration diagnosis remains
available from the database/operator side; production log retention and alert
review are still outstanding.

A follow-up scan also found four Guest/init entrypoints calling `log.Fatal`
with the returned startup error. They now log only
`failure_class=process_exit`, so paths, endpoint details, and provider errors
from Agent/Workspace/Actions Runner startup cannot enter process logs. Their
command packages compile and test locally; deployed log retention remains an
operational review item.

The deferred architecture requirements are now recorded as four separate
ADRs: [BYOC](../docs/adr/0010-customer-owned-resource-planes.md),
[multi-Region placement](../docs/adr/0011-multi-region-resource-planes.md),
[persistent development databases](../docs/adr/0012-persistent-development-databases.md),
and [usage metering/billing](../docs/adr/0013-usage-metering-and-billing.md).
Each names an accountable owner role, scope, requirements, non-goals, and
revisit conditions. They defer implementation and do not approve AWS changes.

## Workspace Service request correlation (2026-10-25)

Workspace Service add/action requests now carry the server-generated API
request ID in their transactional Resource Plane Outbox command. The signed
current-desired-state lookup includes and echoes that exact ID; the Resource
Plane rejects a changed ID. Its SQS failure log records the validated
correlation and bounded Tenant/Workspace/Plane/Service/revision identifiers,
while malformed commands produce no payload-derived fields and raw provider
errors/configuration never enter the log. The PostgreSQL API integration test
checks the request ID against the Outbox payload and signed resolver response;
runtime unit coverage checks changed-ID rejection and redacted failure logging.
This closes the Workspace Service command path only; broader worker/API log
correlation and deployed log retention remain open.

Full local integration refresh (2026-10-25): `make test-integration-local`
passed every package with the `integration` build tag against isolated schemas
in the already-running PostgreSQL database. This covers the local PostgreSQL
acceptance suite; it does not validate AWS deployment or live MicroVM behavior.

Integration suite stability follow-up (2026-10-25): a package-parallel full
run passed the test bodies but two isolated-schema drops failed with
PostgreSQL `out of shared memory`. Both affected packages passed when run
sequentially, and the complete suite then passed with `-p=1`. The local,
disposable-database, coverage, and CI integration commands now serialize Go
packages. The two schemas created by the failed run were verified as generated
`hako_test_*` schemas and removed; the persistent PostgreSQL container remains
running.

Gateway observability follow-up: an active Session lookup failure already closed
the WSS stream and emitted a redacted failure log, but was indistinguishable
from a normal client disconnect in the lifecycle metric. It now emits the
bounded `session_recheck_failed` outcome. Local tests assert the classification
and ensure the metric contains no Tenant/Workspace identity, ticket, or raw
lookup error. Production CloudWatch retention, alarms, and escalation review
remain operational gates.
