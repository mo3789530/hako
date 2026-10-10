# Task 61: Select a Non-EC2 Workload Runtime

Status: Decision recorded; AWS PoC and deployment validation remain in Task 40. Priority: P0. Depends on: Task 39 substrate-neutral dispatch/result/journal foundations.

## Goal

Choose an execution substrate for Software Factory Workload Runs with EC2 explicitly out of scope. The initial native Job runtime decision is ECS/Fargate for direct-command Jobs only; this does not select the Workspace Runtime. Do not silently retain the superseded EC2 decision in ADR 0006.

## Work

- [x] Compare Hako Lambda MicroVM, standard Lambda, and Fargate. Do not design EC2 instances, EC2-backed worker fleets, AMIs, EBS worker disks, or EC2 lifecycle APIs/permissions.
- [x] Evaluate 1-second to 24-hour Run deadlines, nested container/Podman needs, Tenant isolation, untrusted code, network default-deny, source/artifact transfer, cancellation, cleanup, retry/idempotency, queue visibility, cold start, quota, cost, and operational burden. The 1–24h lifecycle, retry, and queue behavior are delegated to Task 40's PoC; target-Region quota, measured latency, and cost remain pending there.
- [x] Define the trust boundary and the runtime contract for `EnsureRunning`, `Cancel`, `GetStatus`, `CollectResult`, and `Cleanup`; preserve CP/RP account separation and never let the runtime access DSQL or GitHub App secrets.
- [x] Record supported/unsupported job types, limits, local development equivalent, ownership, and failure-recovery requirements. Initial profile supports direct commands in an OCI image; privileged/nested container workloads are unsupported.
- [ ] Measure the Fargate PoC in Task 40 (target-Region quotas, startup, cancellation, artifact flow, cleanup, and isolated smoke Run).
- [x] Record the decision and supersede the historical execution decision in ADR 0006; update Task 40 deployment target.

## Acceptance criteria

- [x] An ADR records the selected non-EC2 runtime, rejected alternatives, limits, isolation model, cancellation/cleanup semantics, IAM boundaries, and remaining risks ([ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)).
- [x] Runtime-specific Terraform and AWS changes are not applied as part of this decision task. No EC2 resource or permission is introduced.
- [x] `HAKO_WORKLOAD_RUNS_ENABLED` and `enable_workload_run_creation` remain default-off until the selected worker path and alarms are deployable and validated.
- [x] Task 40 has an unambiguous deployment target and test plan: one ECS/Fargate task per Run attempt, with nested containers excluded and a measured AWS PoC pending.
