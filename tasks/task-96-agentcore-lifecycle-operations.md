# Task 96: AgentCore Lifecycle, Deployment and Operations

Status: In Progress. The Resource Plane now has a default-off ARM64 AgentCore SQS Worker Lambda composed from the bounded handler, mTLS Broker task client, S3 journal/source/artifact stores, official AgentCore SDK invoker/stopper, and SQS result publisher. Terraform scopes queue, Secrets, S3 prefixes, KMS keys, Runtime ARNs, logs and optional VPC permissions; dev outputs expose the function and mapping. Module/dev `tofu validate` and the ARM64 ZIP build pass. A cancellable background heartbeat now renews the durable attempt lease during long invocation/cleanup/publication work and cancels the worker if renewal is lost. AgentCore proposals flow through the standard `WorkloadResult` `ChangeBundle` contract into the existing Control Plane independent-verifier enqueue under factory opt-in; the result bridge and PostgreSQL verifier flow are tested separately. Remaining: Runtime image creation and deployed IAM/network/isolation/cleanup evidence. Priority: P3.
Parent: Task 90. Depends on Tasks 39, 89 and 91–95.

## Goal

Make the selected AgentCore implementation operable with durable, fenced
invocation, observable progress, bounded recovery and verified cleanup.

## Decisions and work

1. Define durable admission, attempt/session mapping, exclusive lease, progress,
   terminal states, cancellation, timeout, duplicate delivery and recovery.
   Specify ambiguous invoke/stop responses and ensure a stopped session cannot
   be accidentally restarted after the attempt is terminal. Reconcile in-flight
   invocation before recording cleanup confirmation.
2. Implement an AgentCore adapter and Python Runtime entrypoint using trusted
   source/task/result contracts. Authenticate and revalidate pins on every
   handoff; keep default-off until Tasks 94 and 95 gates are met.
3. Define durable event and artifact limits, retention, redaction, idempotency,
   alarms, model/token/compute budgets, rate limits and per-tenant quotas. Make
   progress and failures visible through existing Hako APIs without raw secrets.
4. Provide digest-pinned ARM64 image and default-off Terraform/IAM/network
   contracts. Document supported region, provisioning and teardown order,
   retained resources, ongoing costs and rollback.
5. Compose fake-provider integration tests from admission to cleanup, verifier
   and publication eligibility. Include retries, duplicate requests, lost
   responses, stale/cross-Tenant results, cancellation and cleanup failure.
   Provide a separate opt-in AWS smoke procedure and record evidence.

## Acceptance criteria

- No result authorizes publication until current-attempt identity, cleanup and
  independent digest-bound verification all pass.
- Local tests exercise successful and failure recovery with no live AWS/GitHub.
- AWS execution remains disabled until runtime, identity, effective isolation,
  egress, resource limits and teardown are confirmed in the target environment.


Local invocation core follow-up (2026-11-22): `InvokeOnce` now composes `PreparedInvocationFromJournal`, the CAS invocation claim, the digest-checking Runtime request builder, and an injected provider boundary. Fake tests prove pinned target selection, reject foreign request identity before side effects, and ensure a lost provider response is never replayed. `go test ./internal/workloadagentcore ./internal/workloadjournal -count=1` passes. The caller-owned exclusive lease is still required around cancellation and invocation; no AWS call or deployed cleanup evidence exists.

Local result-validation follow-up (2026-11-22): `DecodeInvocationResponse` now binds all response pins to the exact input request, rejects duplicate/unknown JSON keys, validates draft status/iteration/tree digest and verifies proposal SHA-256 before exposing bytes. Fake malformed-result tests pass with the Workload AgentCore unit suite. Independent verification, authenticated storage/handoff, cleanup, and provider deployment remain open.

Local invocation-composition follow-up (2026-11-22): `InvokeOnce` now returns a typed, attempt-bound `InvocationDraft` only after strict response decoding and bounded ZIP inspection; it no longer exposes raw provider bytes. Malformed responses are treated as ambiguous after the durable claim and cannot be replayed. Fake tests cover valid no-proposal response, invalid-response fencing, and lost-response fencing. `go test ./internal/workloadagentcore ./internal/workloadsource ./internal/workloadjournal -count=1` passes. Artifact persistence, cancellation lease/reconciliation, session cleanup, result publication and AWS adapter remain open.

Local Workload result bridge follow-up (2026-11-22): added `workloadcontroller.AgentCoreWorkloadResult`. It checks all journal/Repository/base-commit/source pins against the trusted Workload specification, re-inspects a proposal ZIP, stores it through the existing attempt-scoped Agent artifact writer, and returns only the existing terminal result metadata. The ordinary Workload Controller remains responsible for durable result journaling, session cleanup confirmation, and Control Plane publication. Fake tests cover no-proposal, attempt-scoped proposal storage, foreign pins, invalid output, and deadline rejection. `make test-workload-runtime`, tagged Workload integration tests, and `git diff --check` pass. AgentCore invoke/stop dispatch, shared attempt lease/cancellation fencing, and the deployed S3/IAM adapter remain unconnected.

Local stop/cleanup follow-up (2026-11-22): added terminal-journal restoration through `PreparedCleanupFromJournal` and `StopAndConfirmCleanup`. Stop retries use the same attempt-derived client token; provider errors move cleanup to failed/unconfirmed; only a successful injected stop followed by journal CAS records `CleanupConfirmed`. Fake tests cover failed-stop recovery, stable token, frozen target, idempotent confirmed state, and foreign/nonterminal rejection. Focused Workload AgentCore tests pass; shared invocation/cancellation lease, concrete AWS provider semantics, and deployed cleanup remain open.


Local attempt-lease follow-up (2026-11-22): `workloadjournal` now stores a per-attempt owner/expiry in the same CAS-protected journal and provides acquire/renew/release operations. Stale owners cannot renew or release a renewed/successor lease. `InvokeOnce` and `StopAndConfirmCleanup` require a currently held lease, so invocation and stop entrypoints share the serialization boundary. `make test-workload-runtime`, AgentCore/workloadjournal race tests, tagged Workload integration tests, and `git diff --check` pass. A production worker still needs to renew and hold this lease across cancellation, result checkpoint, cleanup, and publication; no AWS call was made.

Local AWS provider adapter follow-up (2026-11-22): added `AWSRuntimeProvider` over the official AWS SDK for Go AgentCore package. It sends only the frozen Runtime ARN, qualifier and journal session ID; invocation and stop SDK calls set `RetryMaxAttempts=1`, since invocation has no provider idempotency key and ambiguous outcomes must not be replayed automatically. Invocation response streaming is closed and capped at 12 MiB; cleanup accepts only a successful status for the requested session and preserves the deterministic client token. Fake SDK tests cover exact request pins, retry configuration, oversized/non-200/ambiguous results, and foreign-session cleanup rejection. `go test ./internal/workloadagentcore -count=1` passes. No AWS API was called. Lambda/SQS composition, lease renewal, and deployed behavior remain open.

Journal composition correction (2026-11-22): AgentCore's frozen Runtime/session target now uses separate `runtime_specification_payload/hash` fields. The Workload Controller's existing `specification_payload/hash` remains intact for its trusted Workload/source metadata and result recovery. A regression test proves both immutable specifications coexist and the AgentCore target still restores correctly. Focused Go and race tests pass.


Local worker composition follow-up (2026-11-22): `AgentCoreWorkloadWorker` now composes the lifecycle under one journal lease: load trusted Workload/Runtime specs, check cancellation/deadline, resolve pinned invocation input, invoke at most once, store the result draft, persist terminal cleanup-pending state, stop and confirm cleanup, publish, and CAS `ResultPublished`. Lost invocation responses and recovered `Running` attempts fail closed, cleanup retries never reinvoke, and a duplicate published delivery is a no-op. Tests cover success ordering, duplicate delivery, cleanup retry with stable token, and lost response. Focused `go test ./internal/workloadcontroller -run '^TestAgentCoreWorkloadWorker' -count=1` passes. Remaining: SQS/Lambda dispatch and concrete AWS invoker/stopper/source resolver, continuous background lease renewal, independent verifier integration, and deployed IAM/isolation/cleanup evidence.


Local SQS handler follow-up (2026-11-22): added `AgentCoreLambdaHandler` to pass bounded SQS attempt identities to `AgentCoreWorkloadWorker` and return AWS partial-batch failures per message. The decoder rejects duplicate/unknown/missing fields, trailing JSON, oversized records, and invalid identities; failure logs contain only message ID and a fixed failure class. Fake tests cover success plus an isolated retryable message and malformed batches. `go test ./internal/workloadcontroller -run 'AgentCoreLambda|DecodeAgentCoreSQS' -count=1` passes. The handler is not yet composed into a Lambda binary or fed by a durable dispatcher/publisher.


Local lease renewal follow-up (2026-11-22): the worker now renews its journal lease before the synchronous InvokeAgentRuntime call and again at terminal-result checkpoint, cleanup, and publication boundaries. The lease minimum is 20 minutes to cover AgentCore's 15-minute synchronous invocation ceiling plus a five-minute margin; configuration is bounded to 30 minutes. The provider may consume the full 15 minutes and the fake-clock test verifies Stop sees the renewed expiry afterward. Focused worker/AgentCore tests, race tests for Worker/AgentCore/journal, tagged Workload integration tests, `go vet ./...`, and `git diff --check` pass. This is phase-boundary renewal only: no heartbeat runs while source resolution, artifact persistence, cleanup or publication is in progress. A stale lease fails closed when the next CAS renewal is attempted. No AWS API was called.


Task 90/96 Worker deployment follow-up (2026-11-23): composed `cmd/hako-agentcore-worker-lambda` and its default-off Terraform deployment. The worker loads mTLS material from exact Secrets Manager ARNs, resolves immutable task/source inputs through the Broker and S3, invokes/stops only allowlisted AgentCore Runtime ARNs, stores attempt journal/proposal artifacts under configured S3 prefixes, and publishes metadata-only Workload results. The Lambda has reserved concurrency, bounded timeout/memory, partial batch failure reporting, optional VPC wiring, and scoped IAM. `go test ./cmd/hako-agentcore-worker-lambda -count=1`, module/dev `tofu validate`, and `make build-agentcore-worker-lambda` pass. No AWS API/deployment was used. Background lease heartbeat and verifier composition remain open.

Task 90/96 lease-renewal follow-up (2026-11-23): `AgentCoreWorkloadWorker` now renews its CAS-protected attempt lease on a cancellable background loop while resolving, invoking, checkpointing, stopping and publishing. The interval defaults to one third of the lease TTL; a renewal failure cancels in-flight work and fails closed. A focused fake-journal test holds the provider invocation while observing repeated renewals. `go test ./internal/workloadcontroller -run TestAgentCoreWorkloadWorker -count=1` passes. Runtime image, independent verifier composition and deployed lease/IAM/isolation evidence remain open.


Verifier handoff review (2026-11-23): no verifier runs inside AgentCore. `AgentCoreWorkloadResult` persists only the attempt-bound proposal artifact; the ordinary Control Plane result consumer retains authority and enqueues the separate verifier after applying a successful Agent proposal. Existing local tests cover the result bridge and the CP verifier enqueue/binding flow. This preserves the independent verifier boundary; no AgentCore-specific verifier implementation remains.
