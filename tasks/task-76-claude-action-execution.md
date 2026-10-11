# Task 76: Claude Action ephemeral execution and result lifecycle

Status: Todo. Priority: P2. Parent: Task 73. Depends on: Tasks 40, 41, 52, 74, 75.

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
