# Task 42: Software Factory End-to-End

Status: In Progress (local PostgreSQL → Tenant-specific signed HTTP ingress/HMAC and live Installation-binding authorization → duplicate/conflicting Delivery ID handling → immutable untrusted success, failure, pre-dispatch-cancel, in-flight-cancel, timeout, and artifact-failure Runs → isolated Podman Go tests → redacted artifacts → durable Checks outbox/dispatcher → fake GitHub terminal Check conclusions E2E added and run locally; transient GitHub 503 retry is verified without re-running Jobs. A separate integration test composes the PostgreSQL-backed Scheduler Consumer, placement/attempt Outbox, Resource Plane Dispatcher route, Lambda MicroVM Worker command handler, signed Agent execution, PostgreSQL result apply/replay, and confirmed cleanup. CI requires Podman and the pinned Go image. Resolver uses a synthetic in-memory credential, never AWS Secrets Manager. Worker restart recovery, integrated cancellation, and AWS smoke remain). Priority: P0.

## Goal

Prove one Tenant-owned GitHub Repository can go from installation through one isolated Go test Job to a GitHub Check and retained redacted logs.

## Work

- Add a fake GitHub App/API test environment and a dedicated PostgreSQL schema fixture.
- Exercise installation binding, webhook signature/delivery idempotency, repository lookup, immutable SHA, Workload create/outbox/dispatch, scheduler consumer, worker, isolated Podman execution, artifact metadata, and Check update.
- Include duplicate webhook/SQS delivery, API idempotency replay, bad signature, wrong Tenant/repository, poisoned message/DLQ, cancellation, timeout, artifact/GitHub outage, and worker restart.
- Assert secrets never appear in DB, SQS payload, logs, Check body, or artifacts.
- Keep an opt-in real-AWS smoke test separate from PR CI; do not let test cleanup touch a shared/dev/prod account.

## Acceptance criteria

- Deterministic fake-backed E2E runs in CI using local PostgreSQL and Podman without public GitHub credentials.
- Assertions verify eventual Run state, Check conclusion, redacted log artifact, resource cleanup, and correct Tenant/requester visibility.
- Failure diagnostics include Hako Run/event IDs without credential material.
- AWS smoke prerequisites, dedicated account guard, required approvals, and cleanup report are documented.

## Progress (2026-10-12)

- Added `TestSoftwareFactoryPodmanPostgresAndGitHubCheck` under the `integration` build tag. It uses an isolated schema in the existing PostgreSQL service, receives raw signed webhooks through the production Tenant-specific HTTP handler and live DB Installation-binding authorizer (with a synthetic in-memory webhook credential resolver), checks duplicate and changed-body Delivery ID behavior, invalid-HMAC and unbound-Installation rejection, and creates both successful and deliberately failing immutable-commit untrusted Runs. Both execute in network-disabled read-only Podman containers; test output containing a synthetic credential is redacted before artifact persistence. The production Check consumer then publishes success and failure conclusions to a fake GitHub API. Full cross-Tenant/repository API matrix is covered separately and is not fully composed in this Factory E2E.
- CI installs Podman and pulls `golang:1.27.2-alpine`; `HAKO_E2E_REQUIRE_PODMAN=1` prevents the test from silently skipping in CI. No real GitHub credential, AWS resource, or Secret is used.
- The test exposed and fixed PostgreSQL timestamp precision handling in `workloadjobs.Service` and Podman SELinux/user namespace access for the read-only checkout. The checkout is privately relabeled and remains read-only; the process stays UID/GID 65532.
- Local verification uses `make test-software-factory-e2e`, which requires the pinned Podman image and targets the already-running local PostgreSQL through an isolated schema. CI also requires Podman and runs the complete integration suite.
- Extended the Factory E2E with a second signed delivery whose Go test deliberately fails while printing a synthetic credential. It verifies redaction in failure logs/artifacts and a GitHub `failure` Check alongside the successful Run's `success` Check. The fake Queue enforces `BatchSize=1`; a one-shot fake GitHub 503 leaves that receipt pending, the next invocation retries it, and a separate invocation handles the other Check without rerunning either Job. The same test asserts webhook and job credentials are absent from delivery payloads, DB Check/Outbox metadata, SQS messages, and artifacts.
- Added a third signed delivery cancelled before dispatch. The production `DispatchHandler` observes the durable cancellation, marks the Run cancelled, and does not invoke placement or create a `workload_run_attempt`; the final Check is `cancelled`. The test keeps only the latest durable Check revision per Run for delivery.
- The composed Factory E2E now also verifies that a created Run cannot be fetched under a different Tenant ID and is absent from a same-Tenant list filtered to an unrelated requester. The HTTP/API integration suite separately covers the authenticated route behavior and Tenant membership boundary.
- Added a foreign Repository fixture owned by another Tenant and submitted a validly signed delivery through the first Tenant's Installation. Repository-trigger processing marks the delivery handled without creating a Run, proving that a repository ID registered only under another Tenant cannot cross the event-to-Run boundary.
- `TestExecuteGoTestPersistsTerminalStateAndRedactedLogMetadata` separately covers a timed-out test and artifact upload outage. The main Factory E2E now also creates signed timeout and artifact-failure deliveries, injects deterministic executor/artifact outcomes, and verifies `timed_out` and `failure` Check conclusions; failed upload creates no artifact metadata.
- Added a fourth signed delivery that starts the local executor, requests cancellation while the Run is `running` (advancing the desired-state revision), and signals executor stop. The service writes terminal `cancelled` from the latest revision; the durable Check dispatcher then delivers a second `cancelled` conclusion to the fake GitHub API. The complete signed-webhook → Run → in-flight executor cancellation → Check path passes without persisting an artifact for cancelled work. Fifth/sixth signed deliveries inject timeout and artifact-store outage and verify `timed_out`/`failure` Checks; no artifact metadata is created after the upload failure. These use fake/local executor behavior, not a real Worker cancellation command.
- The real local Podman success/failure path now labels each ephemeral container with Hako Tenant/Run metadata and asserts no matching container remains after completion (`--rm`). The E2E constructs a fresh executor to simulate replacement-worker memory loss, redelivers the same Run after its durable terminal commit, and verifies the DB state rejects execution before a second container starts. This validates local retry/cleanup behavior only, not a deployed Lambda MicroVM worker restart.
- Remaining acceptance includes scheduling/worker wiring on Lambda MicroVM (Tasks 39–41/61/63–65), integrated cancellation/restart/physical cleanup through the actual Lambda worker and MicroVM, and wiring the signed-webhook scenarios to a deployed runtime. Local Podman cleanup and fresh-worker replay rejection are verified above. AWS smoke remains approval-gated.
- Added `TestComposedLambdaWorkerResultAndCleanupProjectsIntoPostgres` under
  the `integration` build tag. It consumes a schedule message through the
  PostgreSQL-backed Scheduler Consumer, creates a placement-pinned attempt and
  Outbox event, routes that event through the Resource Plane Dispatcher to its
  dedicated Workload queue, and passes the exact dispatched command to the
  composed Lambda Workload handler. It then launches the fake Lambda MicroVM,
  authenticates a signed source-install/start/result/log exchange with the
  Agent, applies the result through the production PostgreSQL `ResultHandler`,
  simulates a lost publish acknowledgement, and verifies duplicate-result
  fencing, redacted attempt artifact metadata, terminal Run/attempt state, and
  confirmed MicroVM cleanup. The provider, SQS, Agent process, and
  source/archive storage are local fakes; this does not prove live AWS behavior.
