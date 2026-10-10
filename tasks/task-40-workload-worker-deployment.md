# Task 40: Workload Worker, Queue, and Artifact Deployment

Status: Blocked on Task 39 integration and Task 61 Lambda MicroVM provider/API validation. Priority: P0.

## Goal

Deploy the Resource Plane controller and per-attempt Lambda MicroVM runtime. Task 61 first identifies the concrete provider/API and verifies its capabilities. Current Terraform provisions the encrypted scheduler queue/DLQ and Dispatcher send-only access; the production MicroVM worker path is not deployed. See [ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md).

## Work

- Use one Lambda MicroVM allocation per Run attempt. EC2 workers and Fargate Workload workers are excluded. Verify Docker/containerd/Podman capabilities before claiming support.
- Keep Control Plane orchestration limited to scoped DSQL and dispatch access. Resource Plane execution never connects to DSQL. Keep App private-key reads in the trusted credential service; artifact upload is scoped to its exact attempt.
- Provision the private artifact bucket/prefix, encryption, lifecycle/retention, public access block, and least-privilege bucket policy.
- Configure queue visibility, heartbeat, redrive/DLQ, concurrency, shutdown/drain, and bounded retries consistently with Run timeout.
- Add queue age/depth, DLQ, handler failure, stale Run, and worker capacity alarms. Keep creation default-off until smoke tests pass.

## Acceptance criteria

- Terraform validate/plan succeeds with worker disabled by default and no unrelated IAM broadening. No AWS apply without approval.
- Unit/policy tests verify queue, DSQL, secret, S3, KMS, and provider permission scope.
- Local integration verifies worker startup, shutdown, queue retry, Lambda MicroVM adapter with fake provider dependencies, and Podman command smoke tests; these do not prove production MicroVM isolation.
- An explicitly approved provider PoC verifies MicroVM isolation, default-deny egress, credential scoping, launch/cancel/idempotency, attempt-scoped artifacts, deadlines, and confirmed allocation cleanup.
- Runbook covers deployment order, disabling creation, DLQ redrive, artifact retention, and rollback.
- Run creation remains default-off until worker readiness is validated.
