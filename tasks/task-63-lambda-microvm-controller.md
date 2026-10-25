# Task 63: Lambda MicroVM Controller and Recovery

Status: Implemented locally (controller, official AWS SDK adapter, authenticated Agent transport, Worker/queue composition, and bounded recovery are composed in Task 40). Default-off Terraform, AWS deployment, and live Lambda MicroVM validation remain pending. Priority: P0. Depends on: 61, 62; coordinates with Task 43.

## Work

Result: `internal/workloadcontroller` now supplies Ensure/Poll, a frozen specification hash, durable MicroVM ID binding, official Lambda MicroVMs SDK mapping, replayable result metadata, and injected AgentRunner/ResultPublisher. Cancellation/cleanup is tracked in Task 64; local fault and PostgreSQL tests are in Task 65. The authenticated Agent transport and recovery handler are locally composed by Task 40, with queue and schedule Terraform default-off. AWS deployment and live recovery/runtime behavior remain unverified. See [controller design](../docs/lambda-microvm-workloads.md).

- Map the verified provider API to EnsureRunning/Cancel/GetStatus/CollectResult/Cleanup. Resolve execution specifications from an authenticated trusted source, never from dispatch envelope fields or RP DSQL access.
- Use attempt journal CAS and immutable idempotency token before provider effects. Recover launching/accepted checkpoints after process crash and unknown provider responses.
- Preserve cancel tombstones during launch; stop late allocations; verify old allocation cleanup before permitting replacement. Record a handle without weakening cancellation fencing.
- Implement independent physical cleanup retries and orphan detection. Logical terminal state must not imply allocation deletion.
- Connect command consumer only after accepted checkpoints have a durable recovery path. Connect fenced CP result transport after result/artifact validation.

## Acceptance criteria

- Fake-provider tests cover duplicate launch, cancel/launch races, crash before/after provider response, stale generation, deadline, artifact failure, and cleanup retry.
- RP never connects to DSQL or obtains GitHub App private keys. Untrusted code never executes on the controller host.
- No EC2/Fargate Workload fallback. Provider tests and local Podman do not constitute production MicroVM isolation evidence.
- Task 40 deploys only after controller and recovery are testable; creation remains default-off until runtime and alarms are validated.
