# Task 40: Workload Worker, Queue, and Artifact Deployment

Status: Blocked on Task 39 (Task 36 complete). Priority: P0.

## Goal

Deploy an isolated worker for the Workload scheduler queue using the execution substrate selected by Task 36. The current Terraform provisions the encrypted scheduler queue/DLQ and gives the Dispatcher send-only access; it does not deploy a consumer role or worker.

## Work

- Follow [ADR 0006](../docs/adr/0006-workload-worker-substrate.md): pinned/scanned AMI and OCI image, disposable EC2 per attempt, short-lived CP dispatch/RP command/result handlers, private networking, and durable journal/cleanup.
- Separate IAM roles: CP orchestration has scoped DSQL and dispatch access; RP Controller has scoped EC2 lifecycle, conditional S3 journal, and command/result queue permissions, never DSQL access. Keep App private-key reads in the trusted credential service and grant VM upload only for its exact attempt, never shared bucket-wide credentials or EC2 lifecycle permissions.
- Provision the private artifact bucket/prefix, encryption, lifecycle/retention, public access block, and least-privilege bucket policy.
- Configure queue visibility, heartbeat, redrive/DLQ, max concurrency, shutdown/drain behavior, and bounded retries consistently with Run timeout.
- Add metrics/alarms for queue age/depth, DLQ messages, handler failure, stale Runs, and worker capacity. Keep API feature flag default-off; only expose an explicit opt-in after smoke tests pass.

## Acceptance criteria

- Terraform validate/plan succeeds with worker disabled by default; plan shows no unrelated IAM broadening.
- Unit/policy tests verify queue, DSQL, secret, S3, and KMS scope. No AWS apply is part of this task absent approval.
- A local Podman integration proves worker image startup, graceful shutdown, queue retry, and isolated executor startup using fake dependencies.
- Runbook covers deployment order, disabling creation, DLQ inspection/redrive, artifact retention, and rollback.
- `enable_workload_run_creation` remains default-off and cannot be enabled without worker readiness configuration.
