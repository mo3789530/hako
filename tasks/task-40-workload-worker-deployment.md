# Task 40: Workload Worker, Queue, and Artifact Deployment

Status: Ready after Task 39 foundations; Fargate deployment and AWS PoC not started. Priority: P0.

## Goal

Deploy the Resource Plane controller and per-attempt ECS/Fargate task runtime selected by Task 61. The current Terraform provisions the encrypted scheduler queue/DLQ and gives the Dispatcher send-only access; it does not deploy the Fargate worker path. See [ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md) for supported Job profile and limits. No AWS apply is included without approval.

## Work

- Use one ECS task on Fargate per Run attempt. User-managed EC2 instances, EC2-backed fleets, AMIs, EBS worker disks, and EC2 lifecycle APIs/permissions are explicitly excluded. Do not use privileged or nested containers; initial Jobs run direct commands from a fixed OCI image.
- Keep Control Plane orchestration limited to scoped DSQL and dispatch access. Define least-privilege runtime-specific roles only after Task 61; Resource Plane execution must never connect to DSQL. Keep App private-key reads in the trusted credential service and scope artifact upload to its exact attempt.
- Provision the private artifact bucket/prefix, encryption, lifecycle/retention, public access block, and least-privilege bucket policy.
- Configure queue visibility, heartbeat, redrive/DLQ, max concurrency, shutdown/drain behavior, and bounded retries consistently with Run timeout.
- Add metrics/alarms for queue age/depth, DLQ messages, handler failure, stale Runs, and worker capacity. Keep API feature flag default-off; only expose an explicit opt-in after smoke tests pass.

## Acceptance criteria

- Terraform validate/plan succeeds with worker disabled by default; plan shows no unrelated IAM broadening.
- Unit/policy tests verify queue, DSQL, secret, S3, and KMS scope. No AWS apply is part of this task absent approval.
- Local integration proves worker startup, graceful shutdown, queue retry, Fargate adapter behavior using fake dependencies, and Podman command smoke tests; these do not claim Fargate isolation.
- An explicitly approved AWS PoC verifies per-task isolation, default-deny egress, no task credentials available to untrusted code, launch/cancel/idempotency, attempt-scoped artifacts, deadline handling, and confirmed task cleanup. No EC2 resources or permissions appear in the plan.
- Runbook covers deployment order, disabling creation, DLQ inspection/redrive, artifact retention, and rollback.
- `enable_workload_run_creation` remains default-off and cannot be enabled without worker readiness configuration.
