# Task 76: Claude Action ephemeral execution and result lifecycle

Status: In Progress (guest `/run` runtime and signed identity-bound bootstrap start a one-shot official Actions runner through a fake-tested process consumer. A composed PostgreSQL E2E now covers signed webhook admission→startup worker→model-credential HTTP client/Broker handler→signed Lambda MicroVM guest bootstrap→actual runner process consumer→terminal Check delivery→durable cleanup for success, failure, cancellation, and timeout. The model-credential broker route and DB resolver are real; its test transport supplies a synthetic verified Resource Plane certificate, while Lambda MicroVM launch, runner registration-token issuance, and GitHub providers remain fakes. Configure/start/run failures emit bounded identity+phase+exit metadata. Durable per-attempt GitHub Actions Runner Check state uses transactional admission/status outbox events, the shared Checks dispatcher/consumer, a Control Plane-only mTLS broker token endpoint, and startup-failure terminalization. Terminal webhook cleanup has a bounded scheduled Lambda handler composed into the ARM64 runner binary's `cleanup` mode, using Broker mTLS queue/deregistration APIs and the Lambda MicroVM SDK terminator. A separately opted-in Terraform schedule/function role scopes Secret reads, VPC permissions, and MicroVM lifecycle actions; AWS-mock plan coverage verifies default-off and least-privilege wiring. Pinned image artifact and real AWS/GitHub TLS, IAM, network, and deployment verification remain. No AWS apply or real Secret retrieval was performed). Priority: P2. Parent: Task 73. Depends on: Tasks 40, 41, 52, 74, 75.

## Goal

Run one authorized Claude Code Action workflow job on one isolated Lambda MicroVM-backed ephemeral runner, report the exact job result and clean up every resource on every terminal path.

## Work

- Connect the authorized workflow job to the Task 52 runner lifecycle and Lambda MicroVM workload controller; one runtime accepts at most one job.
- Pass only short-lived job-scoped GitHub registration credentials and the approved Tenant model credential through the secret-safe channel defined by Task 75.
- Set bounded startup, execution and shutdown deadlines; handle cancellation, timeout, runner registration failure, GitHub API throttling and worker restart idempotently.
- Redact and associate logs/artifacts with Tenant/repository/workflow/job/SHA; update the corresponding GitHub Check without cross-run result confusion.
- Deregister the ephemeral runner and destroy the MicroVM/workload on success, failure, cancellation, timeout and partial provisioning failure; retry cleanup durably.

## Acceptance criteria

- Fake runner/controller tests cover at-most-once job assignment, duplicate/reordered events, registration failure, cancellation, timeout, result replay and cleanup retry.
- Wrong job or repository assignment cannot access checkout or model credentials.
- Terminal Check conclusion matches the exact workflow job result and is idempotent under retries.
- Tests prove no runner or credential is reused across jobs/Tenants; Lambda MicroVM remains the execution substrate and EC2 is not introduced.

## Local cleanup progress (2026-10-18)

- The focused lifecycle E2E now uses the production HTTP model-credential client
  and Broker route backed by the PostgreSQL resolver, then the production
  signed bootstrapper, guest `/run` handler, and runner process consumer. A
  synthetic verified SPIFFE certificate exercises Broker authorization at the
  HTTP boundary; it does not claim a real TLS handshake or AWS MicroVM run.
  Success, failure, cancellation, and timeout each assert the exact workflow
  identity and process result before the existing Check/cleanup path.

- Terminal success/failure/cancel/timeout webhook events are composed with the
  real PostgreSQL cleanup queue and worker plus stateful fake MicroVM/runner
  providers. Termination and deregistration each fail once; recreated workers
  recover with durable backoff and verify both resources are absent on success.
- Every worker attempt now uses a fresh claim ID. Store updates additionally
  fence by attempt count and exact lease expiry, rejecting stale completion and
  retry even if the recovered worker has the same name.
- Provider work is bounded within the lease and leaves five seconds for the
  database acknowledgement. Invalid or nearly expired claims cause no provider
  call. The focused local/CI command is
  `make test-actions-runner-lifecycle-integration`.
- The PostgreSQL queue remains a CP/library adapter. The Broker now exposes
  Plane-authenticated claim/complete/retry routes and an HTTP queue client;
  mutation coordinates are resolved server-side and retries use the Broker's
  clock. Worker→HTTP client→Broker→PostgreSQL recovery is locally tested,
  including foreign/expired/modified leases and same-name worker recovery.
- Scheduled cleanup Lambda composition and separate schedule/function IAM are
  implemented; Terraform remains default-off and unapplied. Pinned image, full
  authorized-flow E2E and real AWS/GitHub gates remain open.
