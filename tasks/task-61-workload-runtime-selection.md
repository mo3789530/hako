# Task 61: Select a Non-EC2 Workload Runtime

Status: Ready. Priority: P0. Depends on: Task 39 substrate-neutral dispatch/result/journal foundations.

## Goal

Choose an execution substrate for Software Factory Workload Runs with EC2 explicitly out of scope. Hako's existing Lambda MicroVM direction is the primary candidate, but must be checked against native Job requirements before runtime-specific controller or IAM work proceeds. Do not silently retain the superseded EC2 decision in ADR 0006.

## Work

- Compare only in-scope choices such as Hako Lambda MicroVM, standard Lambda for bounded jobs, and managed non-EC2 services such as Fargate where their security and workload constraints fit. Do not design EC2 instances, EC2-backed worker fleets, AMIs, EBS worker disks, or EC2 lifecycle APIs/permissions.
- Evaluate 1-second to 24-hour Run deadlines, nested container/Podman needs, Tenant isolation, untrusted code, network default-deny, source/artifact transfer, cancellation, cleanup, retry/idempotency, queue visibility, cold start, quota, cost, and operational burden.
- Define the trust boundary and the runtime contract for `EnsureRunning`, `Cancel`, `GetStatus`, `CollectResult`, and `Cleanup`; preserve CP/RP account separation and never let the runtime access DSQL or GitHub App secrets.
- Record supported and unsupported job types, maximum runtime, local development equivalent, deployment/IAM ownership, failure recovery, and measured PoC evidence. If no candidate satisfies requirements, keep Workload execution disabled and record the gap rather than falling back to EC2.
- Replace or supersede the historical execution decision in ADR 0006 and update Task 40 only after the decision is approved.

## Acceptance criteria

- An ADR records the selected non-EC2 runtime, rejected alternatives, limits, isolation model, cancellation/cleanup semantics, IAM boundaries, and remaining risks.
- Runtime-specific Terraform and AWS changes are not applied as part of this decision task. No EC2 resource or permission is introduced.
- `HAKO_WORKLOAD_RUNS_ENABLED` and `enable_workload_run_creation` remain default-off until the selected worker path and alarms are deployable and validated.
- Task 40 has an unambiguous deployment target and test plan, or remains blocked with explicit unmet requirements.
