# Task 40: Workload Worker, Queue, and Artifact Deployment

Status: Blocked on Task 39 and Task 61 (runtime selection reopened; EC2 excluded). Priority: P0.

## Goal

Deploy a worker for the Workload scheduler queue using a supported non-EC2 execution substrate selected by Task 61. The current Terraform provisions the encrypted scheduler queue/DLQ and gives the Dispatcher send-only access; it does not deploy a consumer role or worker. Do not begin runtime-specific deployment until Task 61 is accepted.

## Work

- Follow the runtime decision in Task 61; user-managed EC2 instances, AMIs, EBS worker disks, and EC2 lifecycle APIs/permissions are explicitly excluded. Managed services may be considered only if Task 61 selects them.
- Keep Control Plane orchestration limited to scoped DSQL and dispatch access. Define least-privilege runtime-specific roles only after Task 61; Resource Plane execution must never connect to DSQL. Keep App private-key reads in the trusted credential service and scope artifact upload to its exact attempt.
- Provision the private artifact bucket/prefix, encryption, lifecycle/retention, public access block, and least-privilege bucket policy.
- Configure queue visibility, heartbeat, redrive/DLQ, max concurrency, shutdown/drain behavior, and bounded retries consistently with Run timeout.
- Add metrics/alarms for queue age/depth, DLQ messages, handler failure, stale Runs, and worker capacity. Keep API feature flag default-off; only expose an explicit opt-in after smoke tests pass.

## Acceptance criteria

- Terraform validate/plan succeeds with worker disabled by default; plan shows no unrelated IAM broadening.
- Unit/policy tests verify queue, DSQL, secret, S3, and KMS scope. No AWS apply is part of this task absent approval.
- Local integration proves worker startup, graceful shutdown, queue retry, and isolated executor startup using fake dependencies; it must not claim EC2 isolation.
- Runbook covers deployment order, disabling creation, DLQ inspection/redrive, artifact retention, and rollback.
- `enable_workload_run_creation` remains default-off and cannot be enabled without worker readiness configuration.
