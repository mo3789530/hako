# Task 63: Lambda MicroVM Controller and Recovery

Status: Blocked on Task 61 provider/API validation. Priority: P0. Depends on: 61, 62; coordinates with Task 43.

## Work

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
