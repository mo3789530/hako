# Task 41: GitHub Checks API Results

Status: In Progress (repository-scoped `checks:write` token, safe Checks API client, durable desired Check metadata, transactional result Outbox events, dedicated dispatcher routing, SQS consumer, mTLS broker/client, executable, bounded Retry-After visibility scheduling, and opt-in queue/DLQ/IAM/alarm Terraform implemented locally. The module creates a dedicated ECS/Fargate task role with only Checks-queue and Control Plane DSQL-connect permissions, plus an execution role scoped to logs, ECR auth and one exact ECR image repository, and three explicit broker mTLS Secret ARNs. It defines a digest-pinned ARM64 private-subnet service/task definition, exact DSQL SQL-role bootstrap grants, and optional SNS alarm/OK actions. The consumer parses and unsets ECS-injected PEM environment values at startup while retaining local file support. Unit, broker mTLS, PostgreSQL resolver/delivery/replay, ARM64 image build, and dev Control Plane `tofu validate` checks pass. Secret provisioning/rotation, network policy and AWS deployment remain approval-gated. Priority: P0.

## Goal

Publish Hako Job status to the exact GitHub Repository and immutable commit associated with a Workload Run.

## Work

- Add `checks:write` only when this feature is enabled; keep Installation token scopes least-privilege.
- Persist Check Run ID and the exact repository/commit association as metadata so retries update the same Check rather than create duplicates.
- Report queued/in-progress/completed and bounded summaries for passed, failed, timed out, cancelled, and infrastructure-error outcomes.
- Validate annotation paths/line numbers and cap summary/annotation counts and bytes. Never publish secrets or raw unredacted logs.
- Treat GitHub API rate limits and temporary errors as retryable without re-running the Job; when a safe Retry-After is present, apply a bounded per-message visibility delay.

## Implemented foundation

- `TokenClient.ChecksToken` requests a token for exactly one authorized Repository
  with only `checks:write`. Its cache key is distinct from checkout's
  `contents:read` token; no write token can be returned through checkout.
- `ChecksClient.EnsureCheckRun` creates a Check, updates a persisted Check ID,
  or recovers a create whose response was lost by querying the immutable SHA
  and matching the Run's external ID. It rejects identity mismatches and
  bounds response/payload/annotation sizes.
- Local fake-GitHub tests cover create/update, create recovery, SHA mismatch,
  safe rate-limit errors and annotation validation.
- Migration 000066 stores desired Check identity/status/output and the remote
  Check Run ID without any Installation token. Run creation and state changes
  update this row and enqueue a revisioned `workload_run.github_check_requested`
  Outbox event in the same transaction; the dispatcher can route that event to
  the dedicated `HAKO_GITHUB_CHECKS_QUEUE_URL`.
- The result-state mapping is success, failure, cancellation, and timeout aware.
  Summaries are fixed safe metadata and do not include logs.
- `internal/githubchecks.Consumer` reloads the latest desired Check for the
  event's Tenant/Run, validates its exact identity against the immutable Run,
  requests a Run-pinned `checks:write` token, and acknowledges SQS only after
  the identity-checked Check ID is persisted. API/database failures remain
  retryable without re-running the Job.
- `internal/githubbroker` exposes the Checks credential endpoint only over
  verified Control Plane mTLS. The resolver pins Tenant-owned Runs to their
  active integration/revision/generation and resolves legacy shared Runs only
  when every Tenant-owned pin field is absent; it never falls back between
  modes. The broker selects the exact repository from the active registry.
- `cmd/hako-checks-consumer` wires the dedicated SQS queue, DSQL/PostgreSQL
  store, Checks API and broker mTLS provider. Full `go test ./...` and scoped
  PostgreSQL integration tests for shared/owned identity resolution pass.
- `cmd/hako-checks-consumer/Dockerfile` builds a static ARM64 non-root image for
  ECS/Fargate; `make build-github-checks-consumer-image` produces it locally.
  It starts no user workload and requires only its private runtime
  configuration and certificate files. ECS injects PEM values from explicit
  Secrets Manager references; the process parses then unsets them.
- Terraform can optionally deploy the Fargate service/task definition, private
  subnet attachment, separate execution role, task log group and Secret/KMS
  grants. The service flag defaults off and requires an existing ECS cluster,
  pre-created network controls, digest-pinned ARM64 ECR image and its exact
  matching repository ARN, HTTPS broker URL and three Secret ARNs. Terraform
  references ARNs only; it does not retrieve values or apply AWS.
- PostgreSQL integration covers Run state transitions and transactional Check
  Outbox creation, Dispatcher routing, fake GitHub Check creation, a simulated
  DB failure after the remote create, redelivery recovery/update of that same
  Check, Check ID persistence, and queue ack only after DB success.
- SQS receive now applies a bounded 900-second visibility timeout by default;
  `HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS` can set 1–43200 seconds.
  This does not provision redrive/DLQ, alarms, or AWS IAM.
- On retryable GitHub errors with a validated `RetryAt`, the consumer updates
  only that receipt's SQS visibility (1 second–12 hours) and leaves it
  unacknowledged. Errors without a safe retry time use the queue's configured
  visibility/redrive behavior. Unit tests cover the Retry-After delay and
  bounded SQS visibility API.
- Control Plane Terraform has a separate default-off
  `enable_github_checks_delivery`; it requires the Dispatcher and creates an
  SSE-enabled queue, 14-day DLQ restricted to that source queue, five-receive
  redrive policy, queue-specific Dispatcher send grant, exact-queue consumer
  receive/delete/visibility policy, and age/depth/DLQ alarms. It exports URLs,
  policy ARN, and alarm names. It also creates a dedicated ECS task role,
  attaches only the queue receive policy and DSQL `DbConnect` policy, exports
  that role ARN, and optionally sets SNS alarm/OK actions. The role's DSQL
  database username mapping is documented in the bootstrap SQL template.

This is not yet a deployed feature. The broker and executable are locally
verified, but the dedicated queue, IAM, DLQ/redrive, alarms, and production
certificate/network configuration are not provisioned. The consumer does not
schedule arbitrary retry intervals; failed messages without a safe upstream
hint rely on the queue's visibility/redrive policy. Do not enable production
delivery until those pieces and deployment review are complete. See
[`docs/github-checks.md`](../docs/github-checks.md).

## Next execution steps

1. Provision and review the private ECS cluster, subnets, security group
   egress, mTLS Secrets and broker URL; publish the image to an approved private
   registry and review a plan with the explicit service inputs. AWS apply,
   Secret provisioning/rotation, broker cutover and GitHub App permission
   changes require their own approval.
2. Keep AWS deployment, live GitHub App permission changes, and certificate
   provisioning approval-gated.

## Acceptance criteria

- Fake GitHub API tests cover idempotent create/update, commit mismatch refusal, permission failure, rate limiting, annotations, result mapping, and retry after update failure.
- Database changes use migrations and do not store Installation tokens.
- No Check is attached using a branch name or a Repository ID from an untrusted webhook without Tenant registry validation.
- Docs describe App permission changes and rollback.

## Workload Check state transition integration (2026-10-25)

The PostgreSQL Workload Run lifecycle integration test now advances one Run
from `pending` through `provisioning` and `running` to `cancelled`. It asserts
the durable GitHub Check is `queued`, then `in_progress`, then `completed`,
and that each visible Check state change writes its corresponding Outbox event
with the state transition in the same transaction. The existing test also
checks immutable SHA binding, remote Check ID persistence, duplicate cancel,
terminal result mapping, and rejection of a stale state transition. This is
local database/outbox evidence; the dedicated consumer deployment and live
GitHub delivery remain approval-gated.

## Workload pipeline annotation delivery (2026-10-25)

Native Workload pipeline results can now carry bounded repository diagnostics.
The signed Resource Plane result path rejects unsafe/traversal paths, invalid
line ranges, more than 50 annotations, and annotation JSON above 48 KiB before
database writes. Migration 000189 stores annotations as a JSON array on the
pipeline Check row. The Check consumer reloads that exact row and sends the
annotations with the immutable commit SHA; the PostgreSQL Outbox/Consumer test
verifies the path and line reach a fake GitHub Checks endpoint through retry
and Check ID recovery. Actions Runner Checks remain summary-only because their
diagnostics do not identify repository source lines. No live GitHub request or
AWS deployment was performed.
